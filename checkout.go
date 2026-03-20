package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// runCheckout executes a full checkout flow for one card+store in the given browser tab context.
// ctx must be a chromedp context (a browser tab). Returns the result.
func runCheckout(ctx context.Context, entry CardEntry, buyer BuyerInfo) CheckResult {
	start := time.Now()
	card := entry.Card
	card.FullName = buyer.FirstName + " " + buyer.LastName
	storeURL := entry.Store

	result := CheckResult{
		Index:     entry.Index,
		CardLast4: card.Last4,
		Store:     storeURL,
		Status:    "error",
	}
	defer func() {
		result.ElapsedSeconds = time.Since(start).Seconds()
	}()

	logPrefix := fmt.Sprintf("[%d/%s]", entry.Index, card.Last4)

	// Remove navigator.webdriver flag
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		if _, ok := ev.(*target.EventTargetCreated); ok {
			go func() {
				_ = chromedp.Run(ctx,
					chromedp.Evaluate(`Object.defineProperty(navigator, 'webdriver', {get: () => undefined})`, nil),
				)
			}()
		}
	})

	// Navigate to about:blank to initialize the tab
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		result.ErrorMessage = "failed to init tab: " + err.Error()
		fmt.Printf("%s ERROR: %s\n", logPrefix, result.ErrorMessage)
		return result
	}

	// ── Create checkout via HTTP ──
	fmt.Printf("%s Creating checkout on %s...\n", logPrefix, storeURL)
	checkoutURL, cookies, variantID, checkoutPrice, err := createCheckout(storeURL)
	if err != nil {
		result.ErrorMessage = "checkout creation failed: " + err.Error()
		result.ErrorCode = "CHECKOUT_FAILED"
		fmt.Printf("%s ERROR: %s\n", logPrefix, result.ErrorMessage)
		return result
	}
	result.CheckoutPrice = checkoutPrice
	fmt.Printf("%s Checkout URL ready (%.1fs) — $%.2f\n", logPrefix, time.Since(start).Seconds(), checkoutPrice)

	// ── Inject cookies and navigate to checkout ──
	parsedStore, _ := url.Parse(storeURL)
	cookieDomain := parsedStore.Hostname()
	domParts := strings.Split(cookieDomain, ".")
	if len(domParts) > 2 {
		cookieDomain = strings.Join(domParts[len(domParts)-2:], ".")
	}
	cookieDomain = "." + cookieDomain

	for _, c := range cookies {
		expr := network.SetCookie(c.Name, c.Value).WithDomain(cookieDomain).WithPath("/")
		chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			return expr.Do(ctx)
		}))
	}

	if err := chromedp.Run(ctx, chromedp.Navigate(checkoutURL)); err != nil {
		result.ErrorMessage = "failed to navigate to checkout: " + err.Error()
		fmt.Printf("%s ERROR: %s\n", logPrefix, result.ErrorMessage)
		return result
	}

	// Wait for checkout form
	checkoutReady := false
	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Second)
	if err := chromedp.Run(waitCtx,
		chromedp.WaitVisible(`input#email, input[autocomplete="email"], input[name="email"]`, chromedp.ByQuery),
	); err == nil {
		checkoutReady = true
	}
	waitCancel()

	var currentURL string
	chromedp.Run(ctx, chromedp.Location(&currentURL))

	if !checkoutReady || (!strings.Contains(currentURL, "/checkouts/") && !strings.Contains(currentURL, "/checkout")) {
		fmt.Printf("%s Redirected to homepage — recreating cart in browser\n", logPrefix)
		addURL := fmt.Sprintf("%s/cart/add?id=%d&quantity=1", storeURL, variantID)
		if err := chromedp.Run(ctx,
			chromedp.Navigate(addURL),
			chromedp.Sleep(500*time.Millisecond),
		); err != nil {
			result.ErrorMessage = "failed to add to cart in browser"
			fmt.Printf("%s ERROR: %s\n", logPrefix, result.ErrorMessage)
			return result
		}
		if err := chromedp.Run(ctx, chromedp.Navigate(storeURL+"/checkout")); err != nil {
			result.ErrorMessage = "failed to navigate to checkout (retry)"
			fmt.Printf("%s ERROR: %s\n", logPrefix, result.ErrorMessage)
			return result
		}
		retryWait, retryCancel := context.WithTimeout(ctx, 5*time.Second)
		if err := chromedp.Run(retryWait,
			chromedp.WaitVisible(`input#email, input[autocomplete="email"], input[name="email"]`, chromedp.ByQuery),
		); err != nil {
			retryCancel()
			result.ErrorMessage = "checkout form not found on retry"
			fmt.Printf("%s ERROR: %s\n", logPrefix, result.ErrorMessage)
			return result
		}
		retryCancel()
	}

	// ── Fill checkout form ──
	fmt.Printf("%s Filling checkout form...\n", logPrefix)
	fillJS := buildFillJS(buyer)
	var fillResult string
	if err := chromedp.Run(ctx, chromedp.Evaluate(fillJS, &fillResult)); err != nil {
		result.ErrorMessage = "failed to fill form: " + err.Error()
		fmt.Printf("%s ERROR: %s\n", logPrefix, result.ErrorMessage)
		return result
	}
	fmt.Printf("%s Form: %s\n", logPrefix, fillResult)

	// Dismiss autocomplete
	chromedp.Run(ctx, chromedp.KeyEvent("\x1b"))
	chromedp.Run(ctx, chromedp.Evaluate(`document.body.click()`, nil))
	time.Sleep(100 * time.Millisecond)

	// Fix country if needed
	countryFixJS := buildCountryFixJS(buyer.Country)
	var countryStatus string
	chromedp.Run(ctx, chromedp.Evaluate(countryFixJS, &countryStatus))
	if strings.Contains(countryStatus, "not found") || strings.Contains(countryStatus, "not set") {
		selectDropdownViaClick(ctx, buyer.Country, buyer.CountryName)
	}

	time.Sleep(150 * time.Millisecond)

	// Fix state if needed
	stateFixJS := buildStateFixJS(buyer.State)
	var stateStatus string
	chromedp.Run(ctx, chromedp.Evaluate(stateFixJS, &stateStatus))
	if strings.Contains(stateStatus, "not set") {
		selectDropdownViaClick(ctx, buyer.State, buyer.StateName)
	}

	// Auto-fill empty address2
	chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
		function setNativeValue(el, val) {
			const s = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
			s.call(el, val);
			el.dispatchEvent(new Event('input', {bubbles:true}));
			el.dispatchEvent(new Event('change', {bubbles:true}));
			el.dispatchEvent(new Event('blur', {bubbles:true}));
		}
		const sels = ['input[autocomplete="address-line2"]','input[name="address2"]'];
		for(const sel of sels){
			const el = document.querySelector(sel);
			if(el && !el.value){ el.focus(); setNativeValue(el, 'Apt'); return 'filled'; }
		}
		return 'none';
	})()`, nil))

	time.Sleep(200 * time.Millisecond)

	// ── Handle shipping / multi-step ──
	for step := 0; step < 3; step++ {
		if !clickContinueIfPresent(ctx) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	fixValidationErrors(ctx, buyer.Phone)
	time.Sleep(300 * time.Millisecond)

	// ── Fill payment ──
	fmt.Printf("%s Filling payment...\n", logPrefix)
	if err := fillPaymentIframe(ctx, card); err != nil {
		result.ErrorMessage = "payment fill failed: " + err.Error()
		result.ErrorCode = "PAYMENT_FILL_FAILED"
		fmt.Printf("%s ERROR: %s\n", logPrefix, result.ErrorMessage)
		return result
	}

	// Fill billing address if separate
	billingJS := buildBillingJS(buyer)
	chromedp.Run(ctx, chromedp.Evaluate(billingJS, nil))

	// Inject interceptor (no-op, uses Performance API)
	injectFetchInterceptor(ctx)

	// ── Click Pay ──
	fmt.Printf("%s Clicking Pay...\n", logPrefix)
	if err := clickPayButton(ctx); err != nil {
		result.ErrorMessage = "failed to click pay: " + err.Error()
		fmt.Printf("%s ERROR: %s\n", logPrefix, result.ErrorMessage)
		return result
	}

	// Quick card validation check
	time.Sleep(500 * time.Millisecond)
	if cardErr := checkCardValidationError(ctx); cardErr != "" {
		result.Status = "declined"
		result.ErrorCode = "INCORRECT_NUMBER"
		result.ErrorMessage = cardErr
		fmt.Printf("%s DECLINED: %s\n", logPrefix, cardErr)
		return result
	}

	// ── Wait for result ──
	fmt.Printf("%s Waiting for result...\n", logPrefix)
	waitResult := waitForCheckoutResult(ctx, logPrefix)
	result.Status = waitResult.Status
	result.ErrorCode = waitResult.ErrorCode
	result.ErrorMessage = waitResult.ErrorMessage
	result.OrderNumber = waitResult.OrderNumber
	fmt.Printf("%s RESULT: %s (%s)\n", logPrefix, result.Status, result.ErrorCode)
	return result
}

// ──────────────────────────────────────────────────────────────────
// HTTP checkout creation (no browser needed)
// ──────────────────────────────────────────────────────────────────

type productsResponse struct {
	Products []struct {
		ID       int64  `json:"id"`
		Title    string `json:"title"`
		Handle   string `json:"handle"`
		Variants []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
			Price string `json:"price"`
		} `json:"variants"`
	} `json:"products"`
}

func createCheckout(storeURL string) (checkoutURL string, cookies []*http.Cookie, variantID int64, price float64, err error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Fetch products
	resp, err := client.Get(storeURL + "/products.json")
	if err != nil {
		return "", nil, 0, 0, fmt.Errorf("fetch products: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var data productsResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return "", nil, 0, 0, fmt.Errorf("parse products: %w", err)
	}

	// Find cheapest variant
	var cheapest int64
	cheapestPrice := math.MaxFloat64
	for _, p := range data.Products {
		for _, v := range p.Variants {
			pf, _ := strconv.ParseFloat(v.Price, 64)
			if pf < cheapestPrice && pf > 0 {
				cheapestPrice = pf
				cheapest = v.ID
			}
		}
	}
	if cheapest == 0 {
		return "", nil, 0, 0, fmt.Errorf("no products found")
	}

	// Add to cart
	cartBody := fmt.Sprintf(`{"items":[{"id":%d,"quantity":1}]}`, cheapest)
	req, _ := http.NewRequest("POST", storeURL+"/cart/add.js", strings.NewReader(cartBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		return "", nil, 0, 0, fmt.Errorf("add to cart: %w", err)
	}
	resp.Body.Close()

	// Create checkout
	client.CheckRedirect = nil
	resp, err = client.Get(storeURL + "/checkout")
	if err != nil {
		return "", nil, 0, 0, fmt.Errorf("create checkout: %w", err)
	}
	resp.Body.Close()

	parsedURL, _ := url.Parse(storeURL)
	return resp.Request.URL.String(), jar.Cookies(parsedURL), cheapest, cheapestPrice, nil
}

// ──────────────────────────────────────────────────────────────────
// JS builders (parameterized with buyer/card info)
// ──────────────────────────────────────────────────────────────────

func buildFillJS(b BuyerInfo) string {
	return fmt.Sprintf(`(()=>{
		function setNativeValue(el, val) {
			const nativeInputValueSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
			nativeInputValueSetter.call(el, val);
			el.dispatchEvent(new Event('input', {bubbles: true}));
			el.dispatchEvent(new Event('change', {bubbles: true}));
			el.dispatchEvent(new Event('blur', {bubbles: true}));
		}
		function fill(sel, val) {
			if (!val) return;
			const el = document.querySelector(sel);
			if (el) { el.focus(); setNativeValue(el, val); }
		}
		function setSelect(sel, val) {
			const el = document.querySelector(sel);
			if (el) {
				const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLSelectElement.prototype, 'value').set;
				nativeSetter.call(el, val);
				el.dispatchEvent(new Event('input', {bubbles: true}));
				el.dispatchEvent(new Event('change', {bubbles: true}));
				el.dispatchEvent(new Event('blur', {bubbles: true}));
				return true;
			}
			return false;
		}
		function setCustomSelect(labelText, val, displayVal) {
			const labels = document.querySelectorAll('label, legend, .field__label');
			for(const lbl of labels){
				const lt = lbl.textContent.toLowerCase().trim();
				if(!lt.includes(labelText)) continue;
				const parent = lbl.closest('.field, .form-group, fieldset, [data-address-field]') || lbl.parentElement;
				if(!parent) continue;
				const sel = parent.querySelector('select');
				if(sel){
					const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLSelectElement.prototype, 'value').set;
					nativeSetter.call(sel, val);
					sel.dispatchEvent(new Event('input', {bubbles:true}));
					sel.dispatchEvent(new Event('change', {bubbles:true}));
					sel.dispatchEvent(new Event('blur', {bubbles:true}));
					return true;
				}
				const trigger = parent.querySelector('button, [role="combobox"], [role="listbox"]');
				if(trigger){
					trigger.click();
					setTimeout(()=>{
						const opts = document.querySelectorAll('[role="option"], li[data-value]');
						for(const o of opts){
							if(o.dataset.value === val || o.textContent.trim() === displayVal){
								o.click(); return;
							}
						}
					}, 200);
					return true;
				}
			}
			return false;
		}
		const filled = [];
		fill('input#email, input[autocomplete="email"], input[name="email"], input[type="email"]', %q);
		filled.push('email');
		if(!setSelect('select[autocomplete="country"], select[name="countryCode"], select[name="country"]', %q)){
			setCustomSelect('country', %q, %q);
		}
		filled.push('country');
		fill('input[autocomplete="given-name"], input[name="firstName"], input[name="first_name"]', %q);
		fill('input[autocomplete="family-name"], input[name="lastName"], input[name="last_name"]', %q);
		filled.push('name');
		fill('input[autocomplete="address-line1"], input[name="address1"], input[name="address"]', %q);
		fill('input[autocomplete="address-line2"], input[name="address2"]', %q);
		filled.push('address');
		fill('input[autocomplete="address-level2"], input[name="city"]', %q);
		filled.push('city');
		if(!setSelect('select[autocomplete="address-level1"], select[name="zone"], select[name="province"]', %q)){
			setCustomSelect('state', %q, %q);
		}
		fill('input[autocomplete="address-level1"], input[name="zone"], input[name="province"]', %q);
		filled.push('state');
		fill('input[autocomplete="postal-code"], input[name="postalCode"], input[name="zip"]', %q);
		filled.push('zip');
		fill('input[autocomplete="tel"], input[name="phone"], input[type="tel"]', %q);
		filled.push('phone');
		return 'filled: ' + filled.join(', ');
	})()`,
		b.Email, b.Country, b.Country, b.CountryName,
		b.FirstName, b.LastName,
		b.Address1, b.Address2,
		b.City,
		b.State, b.State, b.StateName, b.State,
		b.Zip,
		b.Phone)
}

func buildCountryFixJS(country string) string {
	return fmt.Sprintf(`(()=>{
		const selectors = [
			'select[autocomplete="country"]',
			'select[name="countryCode"]',
			'select[name="country"]',
		];
		function selectOption(el, opt) {
			el.selectedIndex = opt.index;
			el.dispatchEvent(new Event('input', {bubbles:true}));
			el.dispatchEvent(new Event('change', {bubbles:true}));
			el.dispatchEvent(new Event('blur', {bubbles:true}));
		}
		for(const s of selectors){
			const el = document.querySelector(s);
			if(!el) continue;
			if(el.value === '%s') return 'country already set: '+el.value;
			const usOpt = el.querySelector('option[value="%s"]');
			if(usOpt){
				selectOption(el, usOpt);
				return 'selected via option index: '+usOpt.value;
			}
			const opts = el.querySelectorAll('option');
			for(const o of opts){
				if(o.value && o.value.trim() !== '' && !o.disabled){
					selectOption(el, o);
					return 'selected fallback country: '+o.value+' ('+o.textContent.trim()+')';
				}
			}
		}
		return 'select not found or no matching option';
	})()`, country, country)
}

func buildStateFixJS(state string) string {
	return fmt.Sprintf(`(()=>{
		const sel = document.querySelector('select[autocomplete="address-level1"], select[name="zone"], select[name="province"]');
		if(!sel) return 'no state select';
		if(sel.value && sel.value !== '') return 'state ok: '+sel.value;
		const opt = sel.querySelector('option[value="%s"]');
		if(opt){
			sel.selectedIndex = opt.index;
			sel.dispatchEvent(new Event('input', {bubbles:true}));
			sel.dispatchEvent(new Event('change', {bubbles:true}));
			sel.dispatchEvent(new Event('blur', {bubbles:true}));
			return 'state selected via index: '+opt.value;
		}
		return 'state not set';
	})()`, state)
}

func buildBillingJS(b BuyerInfo) string {
	return fmt.Sprintf(`(()=>{
		function setNativeValue(el, val) {
			const s = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
			s.call(el, val);
			el.dispatchEvent(new Event('input', {bubbles:true}));
			el.dispatchEvent(new Event('change', {bubbles:true}));
			el.dispatchEvent(new Event('blur', {bubbles:true}));
		}
		const billingCtx = document.querySelector('[data-billing-address], [id*="billing"], .section--billing-address, fieldset[data-address-fields]');
		function qb(sel) {
			if (billingCtx) { const e = billingCtx.querySelector(sel); if (e) return e; }
			return null;
		}
		const bFirst = qb('input[autocomplete="given-name"], input[name="firstName"]') ||
			qb('input[name*="first_name"], input[name*="firstName"]');
		if (!bFirst || bFirst.value) return 'no separate billing or already filled';
		const filled = [];
		const fields = billingCtx ? billingCtx.querySelectorAll('input, select') : [];
		fields.forEach(el => {
			const ac = (el.autocomplete||'').toLowerCase();
			const nm = (el.name||'').toLowerCase();
			const tag = el.tagName.toLowerCase();
			if (!el.value || el.value === '') {
				if (ac.includes('given-name') || nm.includes('firstname') || nm.includes('first_name')) {
					setNativeValue(el, %q); filled.push('billing_first');
				} else if (ac.includes('family-name') || nm.includes('lastname') || nm.includes('last_name')) {
					setNativeValue(el, %q); filled.push('billing_last');
				} else if (ac.includes('address-line1') || nm.includes('address1') || nm === 'address') {
					setNativeValue(el, %q); filled.push('billing_addr1');
				} else if (ac.includes('address-line2') || nm.includes('address2')) {
					setNativeValue(el, 'Apt'); filled.push('billing_addr2');
				} else if (ac.includes('address-level2') || nm.includes('city')) {
					setNativeValue(el, %q); filled.push('billing_city');
				} else if (ac.includes('postal-code') || nm.includes('zip') || nm.includes('postalcode')) {
					setNativeValue(el, %q); filled.push('billing_zip');
				} else if (ac.includes('tel') || nm.includes('phone') || el.type === 'tel') {
					setNativeValue(el, %q); filled.push('billing_phone');
				}
			}
			if (tag === 'select') {
				if (ac.includes('country') || nm.includes('country')) {
					el.value = %q; el.dispatchEvent(new Event('change',{bubbles:true})); filled.push('billing_country');
				} else if (ac.includes('address-level1') || nm.includes('zone') || nm.includes('province')) {
					el.value = %q; el.dispatchEvent(new Event('change',{bubbles:true})); filled.push('billing_state');
				}
			}
		});
		return filled.length > 0 ? 'filled: '+filled.join(', ') : 'no billing fields to fill';
	})()`, b.FirstName, b.LastName, b.Address1, b.City, b.Zip, b.Phone, b.Country, b.State)
}

// ──────────────────────────────────────────────────────────────────
// Helper functions (refactored from main_browser.go)
// ──────────────────────────────────────────────────────────────────

func clickContinueIfPresent(ctx context.Context) bool {
	js := `(()=>{
		const btns = document.querySelectorAll('button[type="submit"], button, a.step__footer__continue-btn');
		const patterns = ['continue', 'ship to', 'next step', 'proceed', 'continue to shipping', 'continue to payment'];
		const stopWords = ['pay now', 'complete order', 'place order', 'submit order', 'buy now', 'confirm',
			'additional', 'more', 'expand', 'show', 'toggle', 'select', 'add', 'apply', 'remove', 'change'];
		for(const b of btns){
			const txt = b.textContent.toLowerCase().trim().replace(/\s+/g, ' ');
			if(txt.length > 60) continue;
			if(stopWords.some(sw => txt.includes(sw))) continue;
			if(patterns.some(kw => txt.includes(kw))){
				b.click();
				return 'clicked: '+txt;
			}
		}
		return 'none';
	})()`
	var result string
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &result)); err == nil && result != "none" {
		return true
	}
	return false
}

func selectDropdownViaClick(ctx context.Context, value string, displayName string) {
	clickJS := fmt.Sprintf(`(()=>{
		const sels = document.querySelectorAll('select');
		for(const sel of sels){
			if(sel.offsetParent === null) continue;
			const ac = (sel.autocomplete||'').toLowerCase();
			const nm = (sel.name||'').toLowerCase();
			if(ac.includes('country') || nm.includes('country') || nm.includes('countrycode') ||
			   ac.includes('address-level1') || nm.includes('zone') || nm.includes('province')){
				let opt = sel.querySelector('option[value="%s"]');
				if(!opt){
					const allOpts = sel.querySelectorAll('option');
					for(const o of allOpts){
						if(o.value && o.value.trim() !== '' && !o.disabled){ opt = o; break; }
					}
				}
				if(!opt) continue;
				sel.focus();
				sel.size = 5;
				opt.selected = true;
				sel.size = 1;
				sel.dispatchEvent(new Event('input', {bubbles:true}));
				sel.dispatchEvent(new Event('change', {bubbles:true}));
				sel.dispatchEvent(new Event('blur', {bubbles:true}));
				return 'click-selected: '+opt.value;
			}
		}
		return 'no matching select';
	})()`, value)
	var result string
	if err := chromedp.Run(ctx, chromedp.Evaluate(clickJS, &result)); err == nil {
		if !strings.Contains(result, "no matching") {
			return
		}
	}

	// Fallback: type into native select
	selectors := []string{
		`select[autocomplete="country"]`,
		`select[name="countryCode"]`,
		`select[name="country"]`,
		`select[autocomplete="address-level1"]`,
		`select[name="zone"]`,
		`select[name="province"]`,
	}
	for _, sel := range selectors {
		err := chromedp.Run(ctx,
			chromedp.Click(sel, chromedp.ByQuery, chromedp.NodeVisible),
			chromedp.Sleep(200*time.Millisecond),
		)
		if err != nil {
			continue
		}
		prefix := displayName
		if len(prefix) > 6 {
			prefix = prefix[:6]
		}
		for _, ch := range prefix {
			chromedp.Run(ctx, chromedp.KeyEvent(string(ch)))
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		chromedp.Run(ctx, chromedp.KeyEvent("\r"))
		return
	}
}

func fixValidationErrors(ctx context.Context, phone string) {
	js := fmt.Sprintf(`(()=>{
		function setNativeValue(el, val) {
			const s = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
			s.call(el, val);
			el.dispatchEvent(new Event('input', {bubbles:true}));
			el.dispatchEvent(new Event('change', {bubbles:true}));
			el.dispatchEvent(new Event('blur', {bubbles:true}));
		}
		const fixed = [];
		document.querySelectorAll('input[aria-invalid="true"], input:invalid, .field--error input, .field--error select').forEach(el => {
			const name = (el.name||'').toLowerCase();
			const ac = (el.autocomplete||'').toLowerCase();
			const type = (el.type||'').toLowerCase();
			if ((name.includes('phone') || ac.includes('tel') || type === 'tel') && !el.value) {
				setNativeValue(el, %q); fixed.push('phone');
			}
			if ((name.includes('address2') || ac.includes('address-line2')) && !el.value) {
				setNativeValue(el, ' '); fixed.push('address2');
			}
		});
		return fixed.length > 0 ? 'fixed: '+fixed.join(', ') : 'none';
	})()`, phone)
	chromedp.Run(ctx, chromedp.Evaluate(js, nil))
}

// ──────────────────────────────────────────────────────────────────
// Payment iframe filling
// ──────────────────────────────────────────────────────────────────

func fillPaymentIframe(ctx context.Context, card CardInfo) error {
	// Wait for payment iframe
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`iframe[id*="number"], iframe[src*="number-ltr"]`, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("payment iframe not found: %w", err)
	}

	// Scroll into view
	chromedp.Run(ctx, chromedp.Evaluate(`
		(()=>{
			const iframe = document.querySelector('iframe[id*="number"], iframe[src*="number-ltr"]');
			if(iframe) iframe.scrollIntoView({behavior:'instant', block:'center'});
		})()
	`, nil))
	time.Sleep(200 * time.Millisecond)

	// Try frame-context focus approach
	focused := false
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		var cardFrameID cdp.FrameID
		var findFrame func(ft *page.FrameTree)
		findFrame = func(ft *page.FrameTree) {
			if ft.Frame != nil {
				lower := strings.ToLower(ft.Frame.URL)
				isCardNumber := (strings.Contains(lower, "card-fields") && strings.Contains(lower, "number")) ||
					(strings.Contains(lower, "number") && strings.Contains(lower, "ltr")) ||
					(strings.Contains(lower, "card") && strings.Contains(lower, "number") && strings.Contains(lower, "iframe")) ||
					(strings.Contains(lower, "payment") && strings.Contains(lower, "number")) ||
					(strings.Contains(lower, "credit-card") && strings.Contains(lower, "number")) ||
					(strings.Contains(lower, "shopifycs") && strings.Contains(lower, "number"))
				if isCardNumber && cardFrameID == "" {
					cardFrameID = ft.Frame.ID
					return
				}
			}
			for _, child := range ft.ChildFrames {
				if cardFrameID != "" {
					return
				}
				findFrame(child)
			}
		}
		findFrame(tree)
		if cardFrameID == "" {
			return fmt.Errorf("card number frame not found")
		}

		worldID, err := page.CreateIsolatedWorld(cardFrameID).Do(ctx)
		if err != nil {
			return fmt.Errorf("creating isolated world: %w", err)
		}

		_, exDetail, err := runtime.Evaluate(`
			(()=>{
				const inp = document.querySelector('input[name="number"], input[type="text"], input[autocomplete="cc-number"], input');
				if(inp){ inp.focus(); inp.click(); return 'focused: '+inp.name; }
				return 'no input found';
			})()
		`).WithContextID(runtime.ExecutionContextID(worldID)).Do(ctx)
		if err != nil {
			return fmt.Errorf("focusing card input: %w", err)
		}
		if exDetail != nil {
			return fmt.Errorf("focus exception: %v", exDetail)
		}
		focused = true
		return nil
	})); err != nil {
		// Fallback: mouse click on iframe center
		chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			var boundsJSON string
			chromedp.Evaluate(`
				(()=>{
					const iframe = document.querySelector('iframe[id*="number"], iframe[src*="number-ltr"]');
					if(!iframe) return JSON.stringify([]);
					const r = iframe.getBoundingClientRect();
					return JSON.stringify([r.x, r.y, r.width, r.height]);
				})()
			`, &boundsJSON).Do(ctx)
			var bounds []float64
			if err := json.Unmarshal([]byte(boundsJSON), &bounds); err != nil || len(bounds) < 4 {
				return nil
			}
			cx, cy := bounds[0]+bounds[2]/2, bounds[1]+bounds[3]/2
			chromedp.Evaluate(`
				(()=>{
					const iframe = document.querySelector('iframe[id*="number"], iframe[src*="number-ltr"]');
					if(iframe) iframe.scrollIntoView({behavior:'instant', block:'center'});
				})()
			`, nil).Do(ctx)
			time.Sleep(300 * time.Millisecond)

			chromedp.Evaluate(`
				(()=>{
					const iframe = document.querySelector('iframe[id*="number"], iframe[src*="number-ltr"]');
					if(!iframe) return JSON.stringify([]);
					const r = iframe.getBoundingClientRect();
					return JSON.stringify([r.x + r.width/2, r.y + r.height/2]);
				})()
			`, &boundsJSON).Do(ctx)
			json.Unmarshal([]byte(boundsJSON), &bounds)
			if len(bounds) >= 2 {
				cx, cy = bounds[0], bounds[1]
			}

			input.DispatchMouseEvent(input.MousePressed, cx, cy).
				WithButton(input.Left).WithClickCount(1).Do(ctx)
			input.DispatchMouseEvent(input.MouseReleased, cx, cy).
				WithButton(input.Left).WithClickCount(1).Do(ctx)
			return nil
		}))
		time.Sleep(500 * time.Millisecond)
	}

	// Wait for card field to unlock
	for attempt := 0; attempt < 20; attempt++ {
		var ready string
		chromedp.Run(ctx, chromedp.Evaluate(`
			(()=>{
				const iframe = document.querySelector('iframe[id*="number"], iframe[src*="number-ltr"]');
				if(!iframe) return 'no-iframe';
				const parent = iframe.closest('.card-fields-container, [data-card-fields], [class*="card"]');
				if(parent){
					const loading = parent.querySelector('.loading, [aria-busy="true"], .spinner');
					if(loading && loading.offsetParent !== null) return 'loading';
				}
				const r = iframe.getBoundingClientRect();
				if(r.height < 10) return 'collapsed';
				return 'ready';
			})()`, &ready))
		if ready == "ready" || ready == "no-iframe" {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)

	// Re-focus after unlock if using frame context approach
	if focused {
		chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			tree, err := page.GetFrameTree().Do(ctx)
			if err != nil {
				return nil
			}
			var cardFrameID cdp.FrameID
			var findFrame func(ft *page.FrameTree)
			findFrame = func(ft *page.FrameTree) {
				if ft.Frame != nil {
					lower := strings.ToLower(ft.Frame.URL)
					isCard := (strings.Contains(lower, "card-fields") && strings.Contains(lower, "number")) ||
						(strings.Contains(lower, "number") && strings.Contains(lower, "ltr")) ||
						(strings.Contains(lower, "payment") && strings.Contains(lower, "number"))
					if isCard && cardFrameID == "" {
						cardFrameID = ft.Frame.ID
					}
				}
				for _, child := range ft.ChildFrames {
					if cardFrameID == "" {
						findFrame(child)
					}
				}
			}
			findFrame(tree)
			if cardFrameID == "" {
				return nil
			}
			worldID, err := page.CreateIsolatedWorld(cardFrameID).Do(ctx)
			if err != nil {
				return nil
			}
			runtime.Evaluate(`
				(()=>{
					const inp = document.querySelector('input[name="number"], input[type="text"], input[autocomplete="cc-number"], input');
					if(inp){ inp.focus(); inp.click(); }
				})()
			`).WithContextID(runtime.ExecutionContextID(worldID)).Do(ctx)
			return nil
		}))
	}
	time.Sleep(100 * time.Millisecond)

	// Type card number
	typeKeys := func(value string) {
		for _, ch := range value {
			chromedp.Run(ctx, chromedp.KeyEvent(string(ch)))
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
	}

	typeKeys(card.Number)

	// Verify card number was typed
	cardVerify := verifyCardInput(ctx)
	if len(cardVerify) == 0 {
		time.Sleep(500 * time.Millisecond)
		cardVerify = verifyCardInput(ctx)
	}
	if len(cardVerify) == 0 {
		// Retry with click+focus
		chromedp.Run(ctx, chromedp.KeyEvent("a", chromedp.KeyModifiers(input.ModifierCtrl)))
		chromedp.Run(ctx, chromedp.KeyEvent("\b"))
		time.Sleep(100 * time.Millisecond)
		chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			chromedp.Click(`iframe[id*="number"], iframe[src*="number-ltr"]`, chromedp.ByQuery).Do(ctx)
			time.Sleep(200 * time.Millisecond)
			var nodes []*cdp.Node
			if err := chromedp.Nodes(`iframe[id*="number"], iframe[src*="number-ltr"]`, &nodes, chromedp.ByQuery).Do(ctx); err == nil && len(nodes) > 0 {
				iframeNode := nodes[0]
				if iframeNode.ContentDocument != nil && iframeNode.ContentDocument.Children != nil {
					var findInput func(n *cdp.Node) *cdp.Node
					findInput = func(n *cdp.Node) *cdp.Node {
						if n.NodeName == "INPUT" {
							return n
						}
						for _, child := range n.Children {
							if found := findInput(child); found != nil {
								return found
							}
						}
						return nil
					}
					if inp := findInput(iframeNode.ContentDocument); inp != nil {
						chromedp.Run(ctx, chromedp.MouseClickNode(inp))
						time.Sleep(200 * time.Millisecond)
					}
				}
			}
			return nil
		}))
		time.Sleep(200 * time.Millisecond)
		chromedp.Run(ctx, chromedp.KeyEvent("a", chromedp.KeyModifiers(input.ModifierCtrl)))
		chromedp.Run(ctx, chromedp.KeyEvent("\b"))
		time.Sleep(100 * time.Millisecond)
		typeKeys(card.Number)
	}

	// Tab to expiry, type it
	chromedp.Run(ctx, chromedp.KeyEvent("\t"))
	time.Sleep(50 * time.Millisecond)
	typeKeys(card.ExpMM + card.ExpYY)

	// Tab to CVV, type it
	chromedp.Run(ctx, chromedp.KeyEvent("\t"))
	time.Sleep(50 * time.Millisecond)
	typeKeys(card.CVV)

	// Fill name on card
	fillNameOnCard(ctx, card.FullName)

	return nil
}

func verifyCardInput(ctx context.Context) string {
	var cardVerify string
	chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return nil
		}
		var checkFrame func(ft *page.FrameTree)
		checkFrame = func(ft *page.FrameTree) {
			if ft.Frame != nil && cardVerify == "" {
				worldID, err := page.CreateIsolatedWorld(ft.Frame.ID).Do(ctx)
				if err == nil {
					res, _, err := runtime.Evaluate(`
						(()=>{
							const inp = document.querySelector('input[name="number"], input[autocomplete="cc-number"], input[id*="number"]');
							if(inp) return inp.value;
							return '';
						})()
					`).WithContextID(runtime.ExecutionContextID(worldID)).Do(ctx)
					if err == nil && res != nil && res.Value != nil {
						val := strings.Trim(string(res.Value), `"`)
						if len(val) > 0 {
							cardVerify = val
						}
					}
				}
			}
			for _, child := range ft.ChildFrames {
				if cardVerify == "" {
					checkFrame(child)
				}
			}
		}
		checkFrame(tree)
		return nil
	}))
	return cardVerify
}

func fillNameOnCard(ctx context.Context, name string) {
	var nameFocused string
	chromedp.Run(ctx, chromedp.Evaluate(`
		(()=>{
			const sels = [
				'input[autocomplete="cc-name"]',
				'input[placeholder*="name on card" i]',
				'input[placeholder*="cardholder" i]',
				'input[name*="cardName" i]',
				'input[name*="nameoncard" i]',
				'input[id*="name" i][id*="card" i]',
				'input[aria-label*="name on card" i]',
				'input[data-current-field="name"]',
				'input[autocomplete="ccname"]',
				'input[name="name"]'
			];
			function focusAndSelect(inp) {
				inp.scrollIntoView({behavior:'instant', block:'center'});
				inp.focus();
				inp.click();
				try { inp.select(); } catch(e){}
				try { inp.setSelectionRange(0, 99999); } catch(e){}
				try { document.execCommand('selectAll'); } catch(e){}
			}
			for(const label of document.querySelectorAll('label')){
				try {
					const lt = label.textContent.toLowerCase();
					if(lt.includes('name on card') || lt.includes('cardholder')){
						const forId = label.getAttribute('for');
						const inp = forId ? document.getElementById(forId) : label.querySelector('input');
						if(inp){
							focusAndSelect(inp);
							return 'focused via label: '+lt.trim();
						}
					}
				} catch(e){}
			}
			for(const sel of sels){
				try {
					const el = document.querySelector(sel);
					if(el){
						focusAndSelect(el);
						return 'focused: '+sel;
					}
				} catch(e){}
			}
			return 'not found';
		})()
	`, &nameFocused))

	typeKeys := func(value string) {
		for _, ch := range value {
			chromedp.Run(ctx, chromedp.KeyEvent(string(ch)))
			time.Sleep(5 * time.Millisecond)
		}
	}

	if strings.HasPrefix(nameFocused, "focused") {
		time.Sleep(50 * time.Millisecond)
		// Triple-click to select all, then type
		chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			var boundsJSON string
			chromedp.Evaluate(`
				(()=>{
					const el = document.activeElement;
					if(!el) return JSON.stringify([]);
					const r = el.getBoundingClientRect();
					return JSON.stringify([r.x + r.width/2, r.y + r.height/2]);
				})()
			`, &boundsJSON).Do(ctx)
			var coords []float64
			if err := json.Unmarshal([]byte(boundsJSON), &coords); err == nil && len(coords) == 2 {
				cx, cy := coords[0], coords[1]
				input.DispatchMouseEvent(input.MousePressed, cx, cy).
					WithButton(input.Left).WithClickCount(3).Do(ctx)
				input.DispatchMouseEvent(input.MouseReleased, cx, cy).
					WithButton(input.Left).WithClickCount(3).Do(ctx)
			}
			return nil
		}))
		time.Sleep(100 * time.Millisecond)
		typeKeys(name)
	} else {
		// Try iframe fallback
		chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			tree, err := page.GetFrameTree().Do(ctx)
			if err != nil {
				return nil
			}
			var nameFrameID cdp.FrameID
			var findFrame func(ft *page.FrameTree)
			findFrame = func(ft *page.FrameTree) {
				if ft.Frame != nil && nameFrameID == "" {
					lower := strings.ToLower(ft.Frame.URL)
					if (strings.Contains(lower, "card-fields") || strings.Contains(lower, "card")) &&
						strings.Contains(lower, "name") {
						nameFrameID = ft.Frame.ID
					}
				}
				for _, child := range ft.ChildFrames {
					if nameFrameID == "" {
						findFrame(child)
					}
				}
			}
			findFrame(tree)
			if nameFrameID != "" {
				worldID, err := page.CreateIsolatedWorld(nameFrameID).Do(ctx)
				if err != nil {
					return nil
				}
				runtime.Evaluate(`
					(()=>{
						const inp = document.querySelector('input[name="name"], input[autocomplete="cc-name"], input[type="text"], input');
						if(inp){ inp.focus(); inp.click(); }
					})()
				`).WithContextID(runtime.ExecutionContextID(worldID)).Do(ctx)
				return nil
			}
			return nil
		}))
		time.Sleep(200 * time.Millisecond)
		typeKeys(name)
	}
}

func clickPayButton(ctx context.Context) error {
	js := `(()=>{
		const btns = document.querySelectorAll('button[type="submit"], button, input[type="submit"]');
		const payKeywords = ['pay now', 'complete order', 'place order', 'submit order', 'buy now',
			'confirm order', 'place your order', 'checkout', 'finalize'];
		const stopWords = ['additional', 'method', 'more', 'expand', 'toggle', 'select', 'add tip',
			'apply', 'remove', 'change', 'shop pay', 'paypal', 'gpay', 'apple'];
		for(const b of btns){
			const txt = b.textContent.toLowerCase().trim().replace(/\s+/g, ' ');
			if(txt.length > 50) continue;
			if(stopWords.some(sw => txt.includes(sw))) continue;
			if(payKeywords.some(kw => txt.includes(kw))){
				b.click();
				return 'clicked: '+txt;
			}
		}
		for(const b of btns){
			const txt = b.textContent.toLowerCase().trim().replace(/\s+/g, ' ');
			if(txt.length > 50) continue;
			if(stopWords.some(sw => txt.includes(sw))) continue;
			if(txt === 'pay' || txt.startsWith('pay ') || txt.includes('pay $') || txt.includes('pay €')){
				b.click();
				return 'clicked: '+txt;
			}
		}
		const submits = document.querySelectorAll('button[type="submit"]');
		if(submits.length > 0){
			submits[submits.length-1].click();
			return 'clicked last submit';
		}
		return 'no pay button found';
	})()`
	var result string
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &result)); err != nil {
		return err
	}
	return nil
}

func injectFetchInterceptor(ctx context.Context) {
	// No-op — uses Performance API re-fetch approach
}

func checkCardValidationError(ctx context.Context) string {
	js := `(()=>{
		const body = document.body ? document.body.innerText.toLowerCase() : '';
		if(body.includes('enter a valid card number') || body.includes('invalid card number')) {
			const all = document.querySelectorAll('*');
			for(const el of all){
				if(el.children.length === 0 || el.tagName === 'P' || el.tagName === 'SPAN' || el.tagName === 'DIV'){
					const t = el.textContent.trim().toLowerCase();
					if((t.includes('valid card number') || t.includes('invalid card number')) && t.length < 100){
						return el.textContent.trim();
					}
				}
			}
			return 'Enter a valid card number';
		}
		return '';
	})()`

	var errText string
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &errText)); err == nil && errText != "" {
		return errText
	}

	// Check frames via CDP
	chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return nil
		}
		var checkFrame func(ft *page.FrameTree)
		checkFrame = func(ft *page.FrameTree) {
			if ft.Frame != nil && errText == "" {
				worldID, err := page.CreateIsolatedWorld(ft.Frame.ID).Do(ctx)
				if err == nil {
					res, _, err := runtime.Evaluate(`
						(()=>{
							const body = document.body ? document.body.innerText.toLowerCase() : '';
							if(body.includes('valid card number') || body.includes('invalid card')) {
								return document.body.innerText.trim().substring(0, 200);
							}
							return '';
						})()
					`).WithContextID(runtime.ExecutionContextID(worldID)).Do(ctx)
					if err == nil && res != nil && res.Value != nil {
						val := strings.Trim(string(res.Value), `"`)
						if len(val) > 0 {
							errText = val
						}
					}
				}
			}
			for _, child := range ft.ChildFrames {
				if errText == "" {
					checkFrame(child)
				}
			}
		}
		checkFrame(tree)
		return nil
	}))

	return errText
}

// ──────────────────────────────────────────────────────────────────
// Wait for checkout result (success / declined / 3DS / error)
// ──────────────────────────────────────────────────────────────────

func waitForCheckoutResult(ctx context.Context, logPrefix string) CheckResult {
	result := CheckResult{Status: "error"}
	deadline := time.Now().Add(20 * time.Second) // shorter timeout for batch mode
	lastURL := ""

	for time.Now().Before(deadline) {
		var currentURL string
		if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
			break
		}

		if currentURL != lastURL {
			fmt.Printf("%s URL: %s\n", logPrefix, currentURL)
			lastURL = currentURL
		}

		// Success indicators
		if strings.Contains(currentURL, "thank_you") || strings.Contains(currentURL, "thank-you") ||
			strings.Contains(currentURL, "orders/") || strings.Contains(currentURL, "order-confirmation") ||
			strings.Contains(currentURL, "confirmation") {
			result.Status = "success"
			// Try to extract order number from API responses
			apiResult := readPaymentAPIResponses(ctx)
			if apiResult.OrderNumber != "" {
				result.OrderNumber = apiResult.OrderNumber
			}
			return result
		}

		// Card validation errors
		if cardErr := checkCardValidationError(ctx); cardErr != "" {
			result.Status = "declined"
			result.ErrorCode = "INCORRECT_NUMBER"
			result.ErrorMessage = cardErr
			return result
		}

		// Payment errors
		var errorText string
		chromedp.Run(ctx, chromedp.Evaluate(`
			(()=>{
				const errs = document.querySelectorAll('[role="alert"], .notice--error, .field__message--error, [data-error]');
				return Array.from(errs).map(e => e.textContent.trim()).filter(t => t).join(' | ');
			})()
		`, &errorText))

		if errorText != "" {
			lower := strings.ToLower(errorText)
			if strings.Contains(lower, "order") && strings.Contains(lower, "being processed") {
				// Not an error
			} else if strings.Contains(lower, "captcha") {
				result.Status = "error"
				result.ErrorCode = "CAPTCHA"
				result.ErrorMessage = errorText
				return result
			} else if strings.Contains(lower, "3d secure") || strings.Contains(lower, "3ds") ||
				strings.Contains(lower, "authentication") || strings.Contains(lower, "redirect") {
				// Wait for 3DS — but only up to 20s in batch mode
				if time.Now().Add(20 * time.Second).Before(deadline) {
					// Still have time, wait
				} else {
					result.Status = "3ds"
					result.ErrorCode = "3DS_CHALLENGE"
					result.ErrorMessage = errorText
					return result
				}
			} else if strings.Contains(lower, "issue processing") ||
				strings.Contains(lower, "problem processing") ||
				strings.Contains(lower, "unable to process") ||
				strings.Contains(lower, "payment method") ||
				strings.Contains(lower, "try again") ||
				strings.Contains(lower, "card was declined") ||
				strings.Contains(lower, "transaction failed") {
				result.Status = "declined"
				result.ErrorCode = "CARD_DECLINED"
				result.ErrorMessage = errorText
				// Try to get more specific error from API
				apiResult := readPaymentAPIResponses(ctx)
				if apiResult.ErrorCode != "" {
					result.ErrorCode = apiResult.ErrorCode
				}
				if apiResult.ErrorMessage != "" {
					result.ErrorMessage = apiResult.ErrorMessage
				}
				return result
			} else {
				result.Status = "declined"
				result.ErrorCode = "UNKNOWN_ERROR"
				result.ErrorMessage = errorText
				return result
			}
		}

		time.Sleep(500 * time.Millisecond)
	}

	// Timeout — try reading API responses
	apiResult := readPaymentAPIResponses(ctx)
	if apiResult.Status != "" {
		result.Status = apiResult.Status
		result.ErrorCode = apiResult.ErrorCode
		result.ErrorMessage = apiResult.ErrorMessage
		result.OrderNumber = apiResult.OrderNumber
		return result
	}

	result.ErrorCode = "TIMEOUT"
	result.ErrorMessage = "no result within timeout"
	return result
}

type apiParseResult struct {
	Status       string
	ErrorCode    string
	ErrorMessage string
	OrderNumber  string
}

func readPaymentAPIResponses(ctx context.Context) apiParseResult {
	js := `
		(async () => {
			const entries = performance.getEntriesByType('resource')
				.filter(e => e.name.includes('PollForReceipt') || e.name.includes('SubmitForCompletion'));
			const results = [];
			for (const entry of entries) {
				try {
					const resp = await fetch(entry.name, {
						headers: {'Accept': 'application/json', 'Content-Type': 'application/json'}
					});
					const body = await resp.text();
					results.push({url: entry.name.substring(0, 200), status: resp.status, body: body});
				} catch(e) {
					results.push({url: entry.name.substring(0, 200), error: e.message});
				}
			}
			return JSON.stringify(results);
		})()
	`

	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &raw, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	})); err != nil || raw == "" || raw == "[]" {
		return apiParseResult{}
	}

	var responses []map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &responses); err != nil {
		return apiParseResult{}
	}

	var result apiParseResult
	for _, r := range responses {
		body, ok := r["body"]
		if !ok {
			continue
		}
		bodyStr := fmt.Sprintf("%v", body)

		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(bodyStr), &parsed); err != nil {
			continue
		}
		data, _ := parsed["data"].(map[string]interface{})
		if data == nil {
			continue
		}
		receipt, _ := data["receipt"].(map[string]interface{})
		if receipt == nil {
			continue
		}

		orderStatus, _ := receipt["orderCreationStatus"].(map[string]interface{})
		statusType := ""
		if orderStatus != nil {
			statusType, _ = orderStatus["__typename"].(string)
		}

		if statusType == "OrderCreationSucceeded" {
			result.Status = "success"
			if identity, ok := receipt["orderIdentity"].(map[string]interface{}); ok {
				if buyerID, ok := identity["buyerIdentifier"].(string); ok {
					result.OrderNumber = buyerID
				}
			}
			return result
		}

		// Check for 3DS
		if strings.Contains(bodyStr, "CompletePaymentChallenge") ||
			strings.Contains(bodyStr, "THREE_D_SECURE") ||
			strings.Contains(bodyStr, "ThreeDSecure") {
			result.Status = "3ds"
			result.ErrorCode = "3DS_CHALLENGE"
			result.ErrorMessage = "3D Secure authentication required"
			return result
		}

		// Extract error codes
		result.Status = "declined"
		for searchStr := bodyStr; ; {
			idx := strings.Index(searchStr, `"code":"`)
			if idx < 0 {
				break
			}
			start := idx + 8
			end := strings.Index(searchStr[start:], `"`)
			if end < 0 {
				break
			}
			result.ErrorCode = searchStr[start : start+end]
			searchStr = searchStr[start+end:]
		}
		for searchStr := bodyStr; ; {
			idx := strings.Index(searchStr, `"message":"`)
			if idx < 0 {
				break
			}
			start := idx + 11
			end := strings.Index(searchStr[start:], `"`)
			if end < 0 {
				break
			}
			msg := searchStr[start : start+end]
			if len(msg) > 0 && len(msg) < 200 {
				result.ErrorMessage = msg
			}
			searchStr = searchStr[start+end:]
		}
		if result.ErrorCode != "" {
			return result
		}
	}

	return result
}
