package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// apiKey is a simple shared secret for the scraper to authenticate.
var apiKey string

func init() {
	apiKey = os.Getenv("API_KEY")
}

// RegisterSiteRoutes adds site management endpoints to the mux.
func RegisterSiteRoutes(mux *http.ServeMux, db *DB) {
	mux.HandleFunc("/sites/add", func(w http.ResponseWriter, r *http.Request) {
		handleAddSites(w, r, db)
	})
	mux.HandleFunc("/sites/working", func(w http.ResponseWriter, r *http.Request) {
		handleWorkingSites(w, r, db)
	})
	mux.HandleFunc("/sites/stats", func(w http.ResponseWriter, r *http.Request) {
		handleStats(w, r, db)
	})
	mux.HandleFunc("/sites/export", func(w http.ResponseWriter, r *http.Request) {
		handleExport(w, r, db)
	})
	mux.HandleFunc("/sites/dashboard", func(w http.ResponseWriter, r *http.Request) {
		handleDashboard(w, r, db)
	})
	mux.HandleFunc("/sites/delete", func(w http.ResponseWriter, r *http.Request) {
		handleDeleteSite(w, r, db)
	})
	mux.HandleFunc("/sites/recheck", func(w http.ResponseWriter, r *http.Request) {
		handleRecheckAll(w, r, db)
	})
}

func checkAuth(r *http.Request) bool {
	if apiKey == "" {
		return true // no key configured = open
	}
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+apiKey
}

// POST /sites/add — scraper submits discovered sites
// Body: {"urls": ["https://store1.myshopify.com", ...]}
func handleAddSites(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if !checkAuth(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid JSON: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	// Validate and clean URLs
	clean := make([]string, 0, len(req.URLs))
	for _, u := range req.URLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !strings.Contains(u, "myshopify.com") {
			continue
		}
		// Normalize to https
		if !strings.HasPrefix(u, "http") {
			u = "https://" + u
		}
		clean = append(clean, u)
	}

	if len(clean) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"added":    0,
			"received": 0,
		})
		return
	}

	added, err := db.AddSites(clean)
	if err != nil {
		log.Printf("Error adding sites: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	log.Printf("[api] Added %d/%d new sites", added, len(clean))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"added":    added,
		"received": len(clean),
	})
}

// GET /sites/working?limit=100&offset=0
func handleWorkingSites(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	limit := 100
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	sites, total, err := db.GetWorkingSites(limit, offset)
	if err != nil {
		log.Printf("Error getting working sites: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":  total,
		"limit":  limit,
		"offset": offset,
		"sites":  sites,
	})
}

// GET /sites/stats
func handleStats(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	stats, err := db.GetStats()
	if err != nil {
		log.Printf("Error getting stats: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	total := 0
	for _, v := range stats {
		total += v
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":     total,
		"by_status": stats,
	})
}

// GET /sites/export — download all working sites as plain text (one per line)
func handleExport(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	sites, _, err := db.GetWorkingSites(10000, 0)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Disposition", "attachment; filename=working_sites.txt")
	for _, s := range sites {
		fmt.Fprintf(w, "%s | $%.2f\n", s.URL, s.CheckoutPrice)
	}
}

// POST /sites/delete — delete a site by ID
func handleDeleteSite(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID <= 0 {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}
	if err := db.DeleteSite(req.ID); err != nil {
		log.Printf("Error deleting site %d: %v", req.ID, err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	log.Printf("[api] Deleted site ID %d", req.ID)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
}

// POST /sites/recheck — reset all sites back to pending
func handleRecheckAll(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	reset, err := db.RecheckAllSites()
	if err != nil {
		log.Printf("Error rechecking sites: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	log.Printf("[api] Recheck: reset %d sites to pending", reset)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"reset": reset})
}

// GET /sites/dashboard — HTML page showing stats and working sites
func handleDashboard(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Dashboard supports pagination and full list mode:
	// - /sites/dashboard?limit=500&offset=0
	// - /sites/dashboard?all=1
	limit := 500
	offset := 0
	showAll := r.URL.Query().Get("all") == "1"
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	queryLower := strings.ToLower(query)
	if showAll {
		limit = 10000
		offset = 0
	} else {
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
				limit = n
			}
		}
		if v := r.URL.Query().Get("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}
	}

	stats, err := db.GetStats()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	total := 0
	for _, v := range stats {
		total += v
	}

	var sites []Site
	var workingTotal int
	if queryLower != "" {
		// Search mode: query is matched against all working site URLs.
		allWorking, _, err := db.GetWorkingSites(10000, 0)
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		filtered := make([]Site, 0, len(allWorking))
		for _, s := range allWorking {
			if strings.Contains(strings.ToLower(s.URL), queryLower) {
				filtered = append(filtered, s)
			}
		}
		workingTotal = len(filtered)
		if showAll {
			sites = filtered
			offset = 0
		} else {
			if offset > workingTotal {
				offset = 0
			}
			endIdx := offset + limit
			if endIdx > workingTotal {
				endIdx = workingTotal
			}
			if offset < endIdx {
				sites = filtered[offset:endIdx]
			} else {
				sites = []Site{}
			}
		}
	} else {
		sites, workingTotal, err = db.GetWorkingSites(limit, offset)
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
	}

	start := 0
	end := 0
	if len(sites) > 0 {
		start = offset + 1
		end = offset + len(sites)
	}
	prevOffset := offset - limit
	if prevOffset < 0 {
		prevOffset = 0
	}
	nextOffset := offset + limit
	hasPrev := offset > 0
	hasNext := nextOffset < workingTotal

	under15, _ := db.CountWorkingUnder15()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Shopify Site Manager</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#0f172a;color:#e2e8f0;min-height:100vh}
.container{max-width:1100px;margin:0 auto;padding:24px}
h1{font-size:1.8rem;margin-bottom:24px;color:#f8fafc}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:12px;margin-bottom:32px}
.stat{background:#1e293b;border-radius:12px;padding:18px;text-align:center;border:1px solid #334155}
.stat .num{font-size:2rem;font-weight:700;color:#38bdf8}
.stat .label{font-size:.82rem;color:#94a3b8;margin-top:4px}
.stat.working .num{color:#4ade80}
.stat.dead .num{color:#f87171}
.stat.pending .num{color:#facc15}
.stat.error .num{color:#fb923c}
table{width:100%;border-collapse:collapse;background:#1e293b;border-radius:12px;overflow:hidden;border:1px solid #334155}
th{background:#334155;padding:12px 16px;text-align:left;font-size:.82rem;text-transform:uppercase;letter-spacing:.05em;color:#94a3b8}
td{padding:10px 16px;border-top:1px solid #1e293b;font-size:.9rem}
tr:nth-child(even){background:#1e293b}
tr:nth-child(odd){background:#0f172a}
tr:hover{background:#334155}
a{color:#38bdf8;text-decoration:none}
a:hover{text-decoration:underline}
.price{color:#4ade80;font-weight:600}
.badge{display:inline-block;padding:2px 8px;border-radius:6px;font-size:.75rem;font-weight:600}
.badge.working{background:#065f46;color:#4ade80}
.empty{text-align:center;padding:48px;color:#64748b;font-size:1.1rem}
.refresh{margin-bottom:16px;text-align:right}
.refresh a{background:#334155;padding:8px 16px;border-radius:8px;color:#e2e8f0;font-size:.85rem}
.refresh a:hover{background:#475569;text-decoration:none}
.search{margin:0 0 12px 0;display:flex;gap:8px;flex-wrap:wrap}
.search input{min-width:280px;max-width:520px;flex:1;background:#0f172a;border:1px solid #334155;color:#e2e8f0;border-radius:8px;padding:10px 12px}
.search button{background:#334155;color:#e2e8f0;border:none;border-radius:8px;padding:10px 14px;cursor:pointer}
.search button:hover{background:#475569}
.btn-recheck{background:#7c3aed!important;color:#fff!important}
.btn-recheck:hover{background:#6d28d9!important}
.btn-del{background:#dc2626;color:#fff;border:none;border-radius:6px;padding:4px 10px;cursor:pointer;font-size:.8rem}
.btn-del:hover{background:#b91c1c}
h2{font-size:1.2rem;margin-bottom:12px;color:#f8fafc}
</style>
</head>
<body>
<div class="container">
<h1>🏪 Shopify Site Manager</h1>
<div class="stats">`)

	fmt.Fprintf(w, `<div class="stat"><div class="num">%d</div><div class="label">Total Sites</div></div>`, total)
	fmt.Fprintf(w, `<div class="stat working"><div class="num">%d</div><div class="label">Working</div></div>`, stats["working"])
	fmt.Fprintf(w, `<div class="stat pending"><div class="num">%d</div><div class="label">Pending</div></div>`, stats["pending"])
	fmt.Fprintf(w, `<div class="stat"><div class="num">%d</div><div class="label">Checking</div></div>`, stats["checking"])
	fmt.Fprintf(w, `<div class="stat dead"><div class="num">%d</div><div class="label">Dead</div></div>`, stats["dead"])
	fmt.Fprintf(w, `<div class="stat error"><div class="num">%d</div><div class="label">Errors</div></div>`, stats["error"])
	fmt.Fprintf(w, `<div class="stat working"><div class="num">%d</div><div class="label">Working ≤$15</div></div>`, under15)

	fmt.Fprint(w, `</div>
<div class="refresh">
<a href="/sites/dashboard">↻ Refresh</a> &nbsp; <a href="/sites/export">⬇ Export TXT</a> &nbsp;
<a href="/sites/dashboard?all=1">👁 Show All</a> &nbsp;
<a href="/sites/dashboard?limit=500&offset=0">📄 Paged View</a> &nbsp;
<a href="#" onclick="recheckAll(); return false;" class="btn-recheck">🔄 Recheck All Sites</a>
</div>`)

	fmt.Fprint(w, `<form class="search" method="GET" action="/sites/dashboard">`)
	if showAll {
		fmt.Fprint(w, `<input type="hidden" name="all" value="1">`)
	} else {
		fmt.Fprintf(w, `<input type="hidden" name="limit" value="%d">`, limit)
	}
	fmt.Fprintf(w, `<input type="text" name="q" value="%s" placeholder="Search store URL (e.g. favoryt-brand)">`, query)
	fmt.Fprint(w, `<button type="submit">Search</button>`)
	if queryLower != "" {
		if showAll {
			fmt.Fprint(w, `<a href="/sites/dashboard?all=1" style="display:inline-block;background:#334155;padding:10px 14px;border-radius:8px;color:#e2e8f0;text-decoration:none;">Clear</a>`)
		} else {
			fmt.Fprintf(w, `<a href="/sites/dashboard?limit=%d&offset=0" style="display:inline-block;background:#334155;padding:10px 14px;border-radius:8px;color:#e2e8f0;text-decoration:none;">Clear</a>`, limit)
		}
	}
	fmt.Fprint(w, `</form>`)

	fmt.Fprintf(w, `<h2>Working Sites (%d)</h2>`, workingTotal)
	if showAll {
		if queryLower != "" {
			fmt.Fprintf(w, `<div style="margin-bottom:10px;color:#94a3b8;font-size:.9rem;">Showing all %d matching sites for "%s"</div>`, workingTotal, query)
		} else {
			fmt.Fprintf(w, `<div style="margin-bottom:10px;color:#94a3b8;font-size:.9rem;">Showing all %d sites</div>`, workingTotal)
		}
	} else {
		if queryLower != "" {
			fmt.Fprintf(w, `<div style="margin-bottom:10px;color:#94a3b8;font-size:.9rem;">Showing %d-%d of %d matching sites for "%s"</div>`, start, end, workingTotal, query)
		} else {
			fmt.Fprintf(w, `<div style="margin-bottom:10px;color:#94a3b8;font-size:.9rem;">Showing %d-%d of %d</div>`, start, end, workingTotal)
		}
		fmt.Fprint(w, `<div style="margin-bottom:12px;display:flex;gap:8px;flex-wrap:wrap;">`)
		if hasPrev {
			fmt.Fprintf(w, `<a href="/sites/dashboard?limit=%d&offset=%d&q=%s" style="background:#334155;padding:8px 12px;border-radius:8px;color:#e2e8f0;text-decoration:none;">← Prev</a>`, limit, prevOffset, query)
		}
		if hasNext {
			fmt.Fprintf(w, `<a href="/sites/dashboard?limit=%d&offset=%d&q=%s" style="background:#334155;padding:8px 12px;border-radius:8px;color:#e2e8f0;text-decoration:none;">Next →</a>`, limit, nextOffset, query)
		}
		fmt.Fprint(w, `</div>`)
	}

	if len(sites) == 0 {
		fmt.Fprint(w, `<div class="empty">No working sites found yet. The checker is still processing.</div>`)
	} else {
		fmt.Fprint(w, `<table><thead><tr><th>#</th><th>Store URL</th><th>Price</th><th>Last Checked</th><th>Action</th></tr></thead><tbody>`)
		for i, s := range sites {
			lastChecked := "—"
			if s.LastChecked != nil {
				lastChecked = s.LastChecked.Format("Jan 02 15:04")
			}
			fmt.Fprintf(w, `<tr><td>%d</td><td><a href="%s" target="_blank" rel="noopener">%s</a></td><td class="price">$%.2f</td><td>%s</td><td><button class="btn-del" onclick="deleteSite(%d, this)">✕</button></td></tr>`,
				offset+i+1, s.URL, s.URL, s.CheckoutPrice, lastChecked, s.ID)
		}
		fmt.Fprint(w, `</tbody></table>`)
	}

	fmt.Fprint(w, `</div>
<script>
function deleteSite(id, btn) {
	if (!confirm('Delete this site?')) return;
	btn.disabled = true;
	btn.textContent = '...';
	fetch('/sites/delete', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({id:id})})
		.then(r => r.json())
		.then(d => { if(d.ok) btn.closest('tr').remove(); else { alert('Error'); btn.disabled=false; btn.textContent='✕'; } })
		.catch(() => { alert('Error'); btn.disabled=false; btn.textContent='✕'; });
}
function recheckAll() {
	if (!confirm('Reset ALL sites back to pending for re-checking?')) return;
	fetch('/sites/recheck', {method:'POST'})
		.then(r => r.json())
		.then(d => { alert('Deleted ' + d.deleted + ' dead/error sites, reset ' + d.reset + ' to pending'); location.reload(); })
		.catch(() => alert('Error'));
}
setTimeout(()=>location.reload(),30000);
</script>
</body></html>`)
}
