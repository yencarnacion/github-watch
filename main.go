// main.go
// gh-api-watch: Daily GitHub watcher for Polygon.io, Alpaca, IBKR, Databento (and anything else in queries.yaml).
// - CLI launches a local web UI on http://localhost:8084
// - Requires .env with GITHUB_TOKEN and OPENAI_API_KEY
// - Does nothing until you Save Settings, then Run report.
// - Report drafted by OpenAI and displayed as Markdown with a Raw/Pretty toggle (+ copy button).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"sort"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

const (
	defaultPort          = "8084"
	defaultDaysBack      = 7
	defaultQueriesFile   = "queries.yaml"
	defaultModel         = "gpt-5"
	maxPagesDefault      = 2
	perPageDefault       = 50
	maxConcurrentDetails = 2 // workers for commit/date lookups
)

type AppSettings struct {
	DaysBack         int    `json:"daysBack"`
	OpenAIModel      string `json:"openAIModel"`
	MaxPages         int    `json:"maxPages"`         // safety cap per search
	PerPage          int    `json:"perPage"`          // items per page
	UseCommitCheck   bool   `json:"useCommitCheck"`   // try to verify file recency via Commits API
	IncludeRepoSearch bool  `json:"includeRepoSearch"`// include repo-level searches
	QueriesFile      string `json:"queriesFile"`
}

type SearchQuery struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`   // "code" or "repo"
	Query   string `yaml:"query"`  // raw GitHub search query (no date filter; we apply it for repo)
	Enabled bool   `yaml:"enabled"`
}

type SearchGroup struct {
	Name     string        `yaml:"name"`
	Enabled  bool          `yaml:"enabled"`
	Searches []SearchQuery `yaml:"searches"`
}

type QueriesSpec struct {
	Groups []SearchGroup `yaml:"groups"`
}

type CodeHit struct {
	Group       string    `json:"group"`
	QueryName   string    `json:"queryName"`
	Repository  string    `json:"repository"`
	RepoURL     string    `json:"repoUrl"`
	FilePath    string    `json:"filePath"`
	FileURL     string    `json:"fileUrl"`
	Language    string    `json:"language"`
	RepoPushed  time.Time `json:"repoPushed"`
	CommitDate  time.Time `json:"commitDate"` // if verified
}

type RepoHit struct {
	Group       string    `json:"group"`
	QueryName   string    `json:"queryName"`
	FullName    string    `json:"fullName"`
	HTMLURL     string    `json:"htmlUrl"`
	Description string    `json:"description"`
	PushedAt    time.Time `json:"pushedAt"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Findings struct {
	RunID      string    `json:"runId"`
	SinceISO   string    `json:"sinceIso"`
	DaysBack   int       `json:"daysBack"`
	Generated  string    `json:"generated"`
	CodeHits   []CodeHit `json:"codeHits"`
	RepoHits   []RepoHit `json:"repoHits"`
	Notes      []string  `json:"notes"`
}

type Server struct {
	cfg      AppSettings
	mu       sync.RWMutex
	saved    bool
	markdown string
	raw      Findings
	status   string
	inProgress bool
	lastRunID string
	runsMu    sync.RWMutex
	runs      map[string][]DebugEvent
}

// DebugEvent is a structured, per-request/per-phase log entry.
type DebugEvent struct {
	TS            string `json:"ts"`
	RunID         string `json:"runId"`
	Phase         string `json:"phase"`
	Group         string `json:"group,omitempty"`
	QueryName     string `json:"queryName,omitempty"`
	URL           string `json:"url,omitempty"`
	Page          int    `json:"page,omitempty"`
	Status        int    `json:"status,omitempty"`
	RateRemaining string `json:"rateRemaining,omitempty"`
	RateReset     string `json:"rateReset,omitempty"`
	Note          string `json:"note,omitempty"`
}

func main() {
	// Load .env like python-dotenv
	_ = godotenv.Load()

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	s := &Server{
		cfg: AppSettings{
			DaysBack:          defaultDaysBack,
			OpenAIModel:       defaultModel,
			MaxPages:          maxPagesDefault,
			PerPage:           perPageDefault,
			UseCommitCheck:    true,
			IncludeRepoSearch: true,
			QueriesFile:       defaultQueriesFile,
		},
	}
	s.runs = make(map[string][]DebugEvent)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/get-env", s.handleGetEnv)
	mux.HandleFunc("/api/save-settings", s.handleSaveSettings)
	mux.HandleFunc("/api/get-queries", s.handleGetQueries)
	mux.HandleFunc("/api/save-queries", s.handleSaveQueries)
	mux.HandleFunc("/api/run-report", s.handleRunReport)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/debug", s.handleDebug)
	mux.HandleFunc("/api/runs", s.handleRuns)
	mux.HandleFunc("/api/last-raw", func(w http.ResponseWriter, r *http.Request){
		s.mu.RLock(); defer s.mu.RUnlock()
		writeJSON(w, s.raw)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })

	srv := &http.Server{
		Addr:              "127.0.0.1:" + port,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	url := "http://localhost:" + port + "/"
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = exec.Command("xdg-open", url).Start()
	}()

	log.Printf("gh-api-watch listening on %s", url)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

// ====== UI ======

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	page := `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<title>GH API Watch</title>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<style>
:root{--bg:#0b1020;--fg:#e6e9f0;--muted:#a9b0c3;--card:#141a2b;--acc:#6ea8fe}
*{box-sizing:border-box} body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,-apple-system,Segoe UI,Roboto,Ubuntu}
.container{max-width:1120px;margin:32px auto;padding:0 16px}
h1{font-size:1.6rem;margin:0 0 8px} .sub{color:var(--muted);margin-bottom:24px}
.card{background:var(--card);border:1px solid #1f263d;border-radius:10px;padding:16px;margin-bottom:16px}
label{display:block;margin:8px 0 4px} input[type=number],input[type=text],textarea{width:100%;padding:10px;border:1px solid #2b3553;border-radius:8px;background:#0e1426;color:var(--fg)}
.row{display:grid;grid-template-columns:repeat(4,1fr);gap:12px}
.actions{display:flex;gap:12px;flex-wrap:wrap;margin-top:12px}
button{background:var(--acc);color:#081022;border:0;padding:10px 14px;border-radius:8px;cursor:pointer;font-weight:600}
button.secondary{background:#273150;color:var(--fg)}
button:disabled{opacity:.5;cursor:not-allowed}
.small{font-size:.9rem;color:var(--muted)}
#preview{padding:16px;background:#0e1426;border:1px solid #2b3553;border-radius:8px}
pre{white-space:pre-wrap;word-break:break-word}
hr{border:0;border-top:1px solid #253056;margin:16px 0}
kbd{background:#11182d;border:1px solid #2b3553;border-bottom-color:#1d2743;border-radius:6px;padding:2px 6px}
.badge{display:inline-block;padding:2px 8px;border-radius:999px;background:#223050;color:var(--fg);font-size:.8rem;margin-right:6px}
</style>
</head>
<body>
<div class="container">
  <h1>GH API Watch</h1>
  <div class="sub">Daily watcher for Polygon.io, Alpaca, IBKR, Databento (and anything you put in <code>queries.yaml</code>).</div>

  <div class="card">
    <div><span class="badge" id="envGH">GitHub: …</span><span class="badge" id="envOA">OpenAI: …</span><span class="badge" id="saved">Settings: not saved</span></div>
    <p class="small">Keys must be in <code>.env</code> alongside the binary: <code>GITHUB_TOKEN</code> and <code>OPENAI_API_KEY</code>.</p>
    <div class="row">
      <div>
        <label>Days back</label>
        <input id="daysBack" type="number" min="1" max="365" value="7"/>
      </div>
      <div>
        <label>OpenAI model</label>
        <input id="model" type="text" value="gpt-5" placeholder="e.g. gpt-5"/>
      </div>
      <div>
        <label>Max pages per query</label>
        <input id="maxPages" type="number" min="1" max="10" value="2"/>
      </div>
      <div>
        <label>Per page</label>
        <input id="perPage" type="number" min="10" max="100" value="50"/>
      </div>
    </div>
    <div class="row" style="margin-top:8px">
      <div>
        <label><input id="useCommitCheck" type="checkbox" checked/> Verify file recency via Commits API</label>
      </div>
      <div>
        <label><input id="includeRepoSearch" type="checkbox" checked/> Include repo (README/desc) searches</label>
      </div>
      <div>
        <label>Queries file</label>
        <input id="queriesFile" type="text" value="queries.yaml"/>
      </div>
    </div>
    <div class="actions">
      <button id="saveBtn">Save settings</button>
      <button id="runBtn" disabled>Run report</button>
    </div>
  </div>

  <div class="card">
    <h3>Queries (<code>queries.yaml</code>)</h3>
    <p class="small">Editable. One-click save; changes take effect next run.</p>
    <textarea id="queries" rows="14" spellcheck="false"></textarea>
    <div class="actions">
      <button class="secondary" id="reloadQ">Reload from disk</button>
      <button id="saveQ">Save queries.yaml</button>
    </div>
  </div>

  <div class="card">
    <h3>Report</h3>
    <div class="actions">
      <button class="secondary" id="toggle">Toggle Raw/Pretty</button>
      <button class="secondary" id="copy">Copy Raw Markdown</button>
    </div>
    <p class="small" id="status">Idle.</p>
    <hr/>
    <div id="pretty" style="display:none">
      <div id="preview">No report yet.</div>
    </div>
    <div id="raw">
      <pre id="md">No report yet.</pre>
    </div>
  </div>

  <p class="small"><a href="/api/last-raw" target="_blank">View diagnostics JSON</a></p>
  <p class="small">Links open in a new tab. Queries are executed only when you press <strong>Run report</strong>.</p>
</div>
<script src="https://cdn.jsdelivr.net/npm/marked/marked.min.js"></script>
<script>
async function getEnv(){
  const r = await fetch('/api/get-env'); const j = await r.json();
  document.getElementById('envGH').textContent = 'GitHub: ' + (j.github?'✓ found':'missing');
  document.getElementById('envOA').textContent = 'OpenAI: ' + (j.openai?'✓ found':'missing');
  document.getElementById('saved').textContent = 'Settings: ' + (j.saved?'saved':'not saved');
  document.getElementById('daysBack').value = j.settings.daysBack;
  document.getElementById('model').value = j.settings.openAIModel;
  document.getElementById('maxPages').value = j.settings.maxPages;
  document.getElementById('perPage').value = j.settings.perPage;
  document.getElementById('useCommitCheck').checked = j.settings.useCommitCheck;
  document.getElementById('includeRepoSearch').checked = j.settings.includeRepoSearch;
  document.getElementById('queriesFile').value = j.settings.queriesFile;
  document.getElementById('runBtn').disabled = !j.saved;
}
async function loadQueries(){
  const f = document.getElementById('queriesFile').value;
  const r = await fetch('/api/get-queries?file='+encodeURIComponent(f));
  const t = await r.text(); document.getElementById('queries').value = t;
}
document.getElementById('reloadQ').onclick = loadQueries;

document.getElementById('saveBtn').onclick = async ()=>{
  const payload = {
    daysBack: +document.getElementById('daysBack').value,
    openAIModel: document.getElementById('model').value.trim(),
    maxPages: +document.getElementById('maxPages').value,
    perPage: +document.getElementById('perPage').value,
    useCommitCheck: document.getElementById('useCommitCheck').checked,
    includeRepoSearch: document.getElementById('includeRepoSearch').checked,
    queriesFile: document.getElementById('queriesFile').value.trim()
  };
  const r = await fetch('/api/save-settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)});
  const j = await r.json(); if(j.ok){ await getEnv(); await loadQueries(); }
};

document.getElementById('saveQ').onclick = async ()=>{
  const f = document.getElementById('queriesFile').value;
  const body = document.getElementById('queries').value;
  const r = await fetch('/api/save-queries?file='+encodeURIComponent(f), {method:'POST', body});
  if(r.ok){ alert('Saved ' + f); }
};

let statusTimer;
async function pollStatus(){
  try{
    const r = await fetch('/api/status');
    const j = await r.json();
    document.getElementById('status').textContent = j.status || (j.inProgress? 'Working…' : 'Idle.');
    if(!j.inProgress && statusTimer){ clearInterval(statusTimer); statusTimer = undefined; }
  }catch(e){}
}
document.getElementById('runBtn').onclick = async ()=>{
  document.getElementById('runBtn').disabled = true;
  document.getElementById('status').textContent = 'Starting…';
  statusTimer = setInterval(pollStatus, 700);
  let hadErr = false;
  try{
    const r = await fetch('/api/run-report',{method:'POST'});
    if(!r.ok){
      const txt = await r.text();
      document.getElementById('status').textContent = 'Error: ' + txt;
      hadErr = true;
    } else {
      const j = await r.json();
      document.getElementById('md').textContent = j.markdown || '(empty)';
      document.getElementById('preview').innerHTML = marked.parse(j.markdown || '');
      // Ensure all links open in new tab
      const pv = document.getElementById('preview');
      pv.querySelectorAll('a[href]')?.forEach(a=>{ a.target = '_blank'; a.rel = 'noopener noreferrer'; });
    }
  }catch(e){
    document.getElementById('status').textContent = 'Error: ' + (e && e.message? e.message : e);
    hadErr = true;
  } finally {
    document.getElementById('runBtn').disabled = false;
    if (!hadErr) await pollStatus();
  }
};

document.getElementById('toggle').onclick = ()=>{
  const raw = document.getElementById('raw'); const pretty = document.getElementById('pretty');
  if(raw.style.display==='none'){ raw.style.display='block'; pretty.style.display='none'; }
  else { raw.style.display='none'; pretty.style.display='block'; }
};

document.getElementById('copy').onclick = async ()=>{
  const txt = document.getElementById('md').textContent;
  await navigator.clipboard.writeText(txt);
  alert('Markdown copied to clipboard');
};

getEnv(); loadQueries();
</script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"github":   os.Getenv("GITHUB_TOKEN") != "",
		"openai":   os.Getenv("OPENAI_API_KEY") != "",
		"saved":    s.saved,
		"settings": s.cfg,
	}
	writeJSON(w, resp)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ip := s.inProgress
	st := s.status
	s.mu.RUnlock()
	writeJSON(w, map[string]any{"inProgress": ip, "status": st})
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var in AppSettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// enforce sane bounds
	if in.DaysBack < 1 || in.DaysBack > 365 {
		in.DaysBack = defaultDaysBack
	}
	if in.MaxPages < 1 || in.MaxPages > 10 {
		in.MaxPages = maxPagesDefault
	}
	if in.PerPage < 10 || in.PerPage > 100 {
		in.PerPage = perPageDefault
	}
	if in.OpenAIModel == "" {
		in.OpenAIModel = defaultModel
	}
	s.mu.Lock()
	s.cfg = in
	s.saved = true
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleGetQueries(w http.ResponseWriter, r *http.Request) {
	file := r.URL.Query().Get("file")
	if file == "" {
		file = s.cfg.QueriesFile
	}
	b, err := os.ReadFile(file)
	if err != nil {
		// Initialize with a default spec if not found
		if errors.Is(err, os.ErrNotExist) {
			_ = os.WriteFile(file, []byte(defaultQueriesYAML), 0644)
			b = []byte(defaultQueriesYAML)
		} else {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleSaveQueries(w http.ResponseWriter, r *http.Request) {
	file := r.URL.Query().Get("file")
	if file == "" {
		file = s.cfg.QueriesFile
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := os.WriteFile(file, body, 0644); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleRunReport(w http.ResponseWriter, r *http.Request) {
	if !s.saved {
		http.Error(w, "Save settings first.", 400)
		return
	}
	if os.Getenv("GITHUB_TOKEN") == "" || os.Getenv("OPENAI_API_KEY") == "" {
		http.Error(w, "Missing GITHUB_TOKEN or OPENAI_API_KEY in .env", 400)
		return
	}
	spec, err := loadQueries(s.cfg.QueriesFile)
	if err != nil {
		http.Error(w, "queries.yaml: "+err.Error(), 400)
		return
	}

	runID := newRunID()
	s.mu.Lock()
	s.lastRunID = runID
	s.mu.Unlock()
	emit := s.emitFunc(runID)

	// Compute an adaptive timeout based on how many searches you'll make.
	// Roughly 2.2s/request + margin. Floor 2m, cap 6m.
	totalSearches := 0
	for _, g := range spec.Groups {
		if !g.Enabled { continue }
		for _, q := range g.Searches {
			if q.Enabled { totalSearches++ }
		}
	}
	perReq := 12000 * time.Millisecond
	budget := time.Duration(totalSearches*max(1, s.cfg.MaxPages))*perReq + 60*time.Second
	if budget < 4*time.Minute { budget = 4*time.Minute }
	if budget > 10*time.Minute { budget = 10*time.Minute }
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	emit(DebugEvent{Phase: "start", Note: fmt.Sprintf("budget=%s daysBack=%d maxPages=%d perPage=%d includeRepo=%v commitCheck=%v",
		budget, s.cfg.DaysBack, s.cfg.MaxPages, s.cfg.PerPage, s.cfg.IncludeRepoSearch, s.cfg.UseCommitCheck)})

	// mark progress and expose via /api/status
	s.mu.Lock()
	s.inProgress = true
	s.status = "Running GitHub searches..."
	s.mu.Unlock()
	defer func(){
		s.mu.Lock()
		s.inProgress = false
		s.status = "Done."
		s.mu.Unlock()
	}()

	findings, err := runSearches(ctx, s.cfg, spec, emit)
	if err != nil {
		emit(DebugEvent{Phase: "error", Note: "search phase: " + err.Error()})
		http.Error(w, "search error: "+err.Error(), 500)
		return
	}
	findings.RunID = runID
	emit(DebugEvent{Phase: "search-summary", Note: fmt.Sprintf("codeHits=%d repoHits=%d notes=%d", len(findings.CodeHits), len(findings.RepoHits), len(findings.Notes))})

	// next phase
	s.mu.Lock(); s.status = "Drafting report with OpenAI..."; s.mu.Unlock()
	openAITimeout := 10 * time.Minute
	emit(DebugEvent{Phase: "openai", Note: fmt.Sprintf("model=%s payload=compact timeout=%s", s.cfg.OpenAIModel, openAITimeout)})
	openCtx, openCancel := context.WithTimeout(context.Background(), openAITimeout)
	defer openCancel()
	md, err := draftReportWithOpenAI(openCtx, s.cfg, findings)
	if err != nil {
		// Fallback: return a minimal markdown report so the UI still shows something
		s.mu.Lock(); s.status = "OpenAI failed; returning fallback report."; s.mu.Unlock()
		emit(DebugEvent{Phase: "openai-error", Note: err.Error()})
		md = buildFallbackMarkdown(findings, err)
	}
	if strings.TrimSpace(md) == "" {
		emit(DebugEvent{Phase: "openai-empty", Note: "empty content from OpenAI; using fallback"})
		md = buildFallbackMarkdown(findings, errors.New("empty OpenAI response"))
	}
	emit(DebugEvent{Phase: "done", Note: fmt.Sprintf("markdownLen=%d", len(md))})

	s.mu.Lock()
	s.markdown = md
	s.raw = findings
	s.mu.Unlock()

	writeJSON(w, map[string]any{"markdown": md})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
    s.runsMu.RLock()
    ids := make([]string, 0, len(s.runs))
    for id := range s.runs { ids = append(ids, id) }
    s.runsMu.RUnlock()
    sort.Strings(ids)
    writeJSON(w, map[string]any{"runs": ids, "last": s.lastRunID})
}

func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request) {
    run := r.URL.Query().Get("run")
    if run == "" || run == "last" {
        run = s.lastRunID
    }
    s.runsMu.RLock()
    evs := append([]DebugEvent(nil), s.runs[run]...)
    s.runsMu.RUnlock()
    writeJSON(w, map[string]any{"runId": run, "events": evs})
}

// ====== Queries loader ======

func loadQueries(path string) (*QueriesSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var q QueriesSpec
	if err := yaml.Unmarshal(b, &q); err != nil {
		return nil, err
	}
	// normalize: if no groups specified, return error
	if len(q.Groups) == 0 {
		return nil, errors.New("no groups in queries.yaml")
	}
	return &q, nil
}

// ====== GitHub client & search ======

type ghClient struct {
	token string
}

func newGH() *ghClient {
	return &ghClient{token: os.Getenv("GITHUB_TOKEN")}
}

func (c *ghClient) get(ctx context.Context, url string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return http.DefaultClient.Do(req)
}

type codeSearchResp struct {
	TotalCount        int           `json:"total_count"`
	IncompleteResults bool          `json:"incomplete_results"`
	Items             []codeItem    `json:"items"`
}
type codeItem struct {
	Name       string     `json:"name"`
	Path       string     `json:"path"`
	SHA        string     `json:"sha"`
	HTMLURL    string     `json:"html_url"`
	Repository codeRepo   `json:"repository"`
}
type codeRepo struct {
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
	Language string `json:"language,omitempty"`
}

type repoSearchResp struct {
	TotalCount        int         `json:"total_count"`
	IncompleteResults bool        `json:"incomplete_results"`
	Items             []repoItem  `json:"items"`
}
type repoItem struct {
	FullName    string   `json:"full_name"`
	HTMLURL     string   `json:"html_url"`
	Description string   `json:"description"`
	PushedAt    string   `json:"pushed_at"`
	CreatedAt   string   `json:"created_at"`
	Topics      []string `json:"topics"`
}

type commitResp []struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Date string `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	HTMLURL string `json:"html_url"`
}

func runSearches(ctx context.Context, cfg AppSettings, spec *QueriesSpec, emit func(DebugEvent)) (Findings, error) {
	since := time.Now().Add(-time.Duration(cfg.DaysBack) * 24 * time.Hour).UTC()
	sinceISO := since.Format(time.RFC3339)

	client := newGH()
	var codeHits []CodeHit
	var repoHits []RepoHit
	var notes []string

	perPage := cfg.PerPage
	maxPages := cfg.MaxPages

	// Rate safety handled by throttleFrom()

	for _, g := range spec.Groups {
		if !g.Enabled {
			continue
		}
		for _, q := range g.Searches {
			if !q.Enabled {
				continue
			}
			qName := fmt.Sprintf("%s — %s", g.Name, q.Name)
			switch strings.ToLower(q.Type) {
			case "code":
				page := 1
				foundThisQuery := 0
				for page <= maxPages {
					select {
					case <-ctx.Done():
						return Findings{}, ctx.Err()
					default:
					}
					rawQ := sanitizeCodeQuery(q.Query)
					url := fmt.Sprintf("https://api.github.com/search/code?q=%s&sort=indexed&order=desc&per_page=%d&page=%d",
						urlQueryEscape(rawQ), perPage, page)
					emit(DebugEvent{Phase: "search-code", Group: g.Name, QueryName: q.Name, URL: url, Page: page})
					resp, err := client.get(ctx, url)
					if err != nil {
						emit(DebugEvent{Phase: "search-code-error", Group: g.Name, QueryName: q.Name, URL: url, Page: page, Note: err.Error()})
						return Findings{}, err
					}
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != 200 {
						rlRem := resp.Header.Get("X-RateLimit-Remaining")
						rlRes := resp.Header.Get("X-RateLimit-Reset")
						// If rate-limited, indicate planned sleep until reset
						note := truncate(string(body), 200)
						if resp.StatusCode == 403 || rlRem == "0" {
							if ru, err := strconv.ParseInt(rlRes, 10, 64); err == nil {
								ws := int(time.Until(time.Unix(ru, 0)).Seconds())
								if ws > 0 { note = fmt.Sprintf("rate-limited; sleeping %ds; body=%s", ws, note) }
							}
						}
						emit(DebugEvent{Phase: "search-code-non200", Group: g.Name, QueryName: q.Name, URL: url, Page: page, Status: resp.StatusCode, RateRemaining: rlRem, RateReset: rlRes, Note: note})
						// If GitHub says the query cannot be parsed, retry once with strict escaping
						if resp.StatusCode == 422 {
							strictURL := fmt.Sprintf("https://api.github.com/search/code?q=%s&sort=indexed&order=desc&per_page=%d&page=%d",
								neturl.QueryEscape(strings.TrimSpace(rawQ)), perPage, page)
							emit(DebugEvent{Phase: "search-code-retry", Group: g.Name, QueryName: q.Name, URL: strictURL, Page: page, Note: "retry with QueryEscape due to 422"})
							resp2, err2 := client.get(ctx, strictURL)
							if err2 == nil {
								body2, _ := io.ReadAll(resp2.Body)
								_ = resp2.Body.Close()
								if resp2.StatusCode == 200 {
									var cr2 codeSearchResp
									if err := json.Unmarshal(body2, &cr2); err != nil {
										return Findings{}, err
									}
									if len(cr2.Items) == 0 {
										emit(DebugEvent{Phase: "search-code-ok", Group: g.Name, QueryName: q.Name, URL: strictURL, Page: page, Status: 200, Note: "0 items"})
										break
									}
									for _, it := range cr2.Items {
										hit := CodeHit{
											Group:      g.Name,
											QueryName:  q.Name,
											Repository: it.Repository.FullName,
											RepoURL:    it.Repository.HTMLURL,
											FilePath:   it.Path,
											FileURL:    it.HTMLURL,
											Language:   it.Repository.Language,
										}
										codeHits = append(codeHits, hit)
										foundThisQuery++
									}
									emit(DebugEvent{Phase: "search-code-ok", Group: g.Name, QueryName: q.Name, URL: strictURL, Page: page, Status: 200, Note: fmt.Sprintf("items=%d", len(cr2.Items))})
									page++
									throttleFrom(resp2)
									continue
								}
								// annotate second failure
								notes = append(notes, fmt.Sprintf("(%s) retry strict status=%d remaining=%s reset=%s url=%s body=%s",
									qName, resp2.StatusCode, resp2.Header.Get("X-RateLimit-Remaining"), resp2.Header.Get("X-RateLimit-Reset"), strictURL, truncate(string(body2), 400)))
								emit(DebugEvent{Phase: "search-code-retry-failed", Group: g.Name, QueryName: q.Name, URL: strictURL, Page: page, Status: resp2.StatusCode, RateRemaining: resp2.Header.Get("X-RateLimit-Remaining"), RateReset: resp2.Header.Get("X-RateLimit-Reset"), Note: truncate(string(body2), 200)})
							}
						}
						notes = append(notes, fmt.Sprintf("(%s) status=%d remaining=%s reset=%s url=%s body=%s",
							qName, resp.StatusCode, rlRem, rlRes, url, truncate(string(body), 400)))
						throttleFrom(resp)
						break
					}
					var cr codeSearchResp
					if err := json.Unmarshal(body, &cr); err != nil {
						return Findings{}, err
					}
					if len(cr.Items) == 0 {
						emit(DebugEvent{Phase: "search-code-ok", Group: g.Name, QueryName: q.Name, URL: url, Page: page, Status: 200, Note: "0 items"})
						break
					}
					for _, it := range cr.Items {
						hit := CodeHit{
							Group:      g.Name,
							QueryName:  q.Name,
							Repository: it.Repository.FullName,
							RepoURL:    it.Repository.HTMLURL,
							FilePath:   it.Path,
							FileURL:    it.HTMLURL,
							Language:   it.Repository.Language,
						}
						codeHits = append(codeHits, hit)
						foundThisQuery++
					}
					emit(DebugEvent{Phase: "search-code-ok", Group: g.Name, QueryName: q.Name, URL: url, Page: page, Status: 200, Note: fmt.Sprintf("items=%d", len(cr.Items))})
					page++
					throttleFrom(resp)
				}
				if foundThisQuery == 0 {
					notes = append(notes, fmt.Sprintf("No code hits returned for %s", qName))
					emit(DebugEvent{Phase: "search-code-empty", Group: g.Name, QueryName: q.Name, Note: "no code hits"})
				}
			case "repo":
				if !cfg.IncludeRepoSearch {
					continue
				}
				page := 1
				foundThisQuery := 0
				// Automatically add pushed:>= filter for recency window
				baseQ := fmt.Sprintf("%s pushed:>=%s", q.Query, since.Format("2006-01-02"))
				for page <= maxPages {
					select {
					case <-ctx.Done():
						return Findings{}, ctx.Err()
					default:
					}
					url := fmt.Sprintf("https://api.github.com/search/repositories?q=%s&sort=updated&order=desc&per_page=%d&page=%d",
						urlQueryEscape(baseQ), perPage, page)
					emit(DebugEvent{Phase: "search-repo", Group: g.Name, QueryName: q.Name, URL: url, Page: page})
					resp, err := client.get(ctx, url)
					if err != nil {
						emit(DebugEvent{Phase: "search-repo-error", Group: g.Name, QueryName: q.Name, URL: url, Page: page, Note: err.Error()})
						return Findings{}, err
					}
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != 200 {
						rlRem := resp.Header.Get("X-RateLimit-Remaining")
						rlRes := resp.Header.Get("X-RateLimit-Reset")
						note := truncate(string(body), 200)
						if resp.StatusCode == 403 || rlRem == "0" {
							if ru, err := strconv.ParseInt(rlRes, 10, 64); err == nil {
								ws := int(time.Until(time.Unix(ru, 0)).Seconds())
								if ws > 0 { note = fmt.Sprintf("rate-limited; sleeping %ds; body=%s", ws, note) }
							}
						}
						notes = append(notes, fmt.Sprintf("(%s) status=%d remaining=%s reset=%s url=%s body=%s",
							qName, resp.StatusCode, rlRem, rlRes, url, truncate(string(body), 400)))
						emit(DebugEvent{Phase: "search-repo-non200", Group: g.Name, QueryName: q.Name, URL: url, Page: page, Status: resp.StatusCode, RateRemaining: rlRem, RateReset: rlRes, Note: note})
						throttleFrom(resp)
						break
					}
					var rr repoSearchResp
					if err := json.Unmarshal(body, &rr); err != nil {
						return Findings{}, err
					}
					if len(rr.Items) == 0 {
						emit(DebugEvent{Phase: "search-repo-ok", Group: g.Name, QueryName: q.Name, URL: url, Page: page, Status: 200, Note: "0 items"})
						break
					}
					for _, it := range rr.Items {
						pushed, _ := time.Parse(time.RFC3339, it.PushedAt)
						created, _ := time.Parse(time.RFC3339, it.CreatedAt)
						if pushed.Before(since) {
							continue
						}
						repoHits = append(repoHits, RepoHit{
							Group:       g.Name,
							QueryName:   q.Name,
							FullName:    it.FullName,
							HTMLURL:     it.HTMLURL,
							Description: it.Description,
							PushedAt:    pushed,
							CreatedAt:   created,
						})
						foundThisQuery++
					}
					emit(DebugEvent{Phase: "search-repo-ok", Group: g.Name, QueryName: q.Name, URL: url, Page: page, Status: 200, Note: fmt.Sprintf("items=%d", len(rr.Items))})
					page++
					throttleFrom(resp)
				}
				if foundThisQuery == 0 {
					notes = append(notes, fmt.Sprintf("No repo hits for %s", qName))
					emit(DebugEvent{Phase: "search-repo-empty", Group: g.Name, QueryName: q.Name, Note: "no repo hits"})
				}
			default:
				notes = append(notes, fmt.Sprintf("Unknown type for %s: %s", qName, q.Type))
				emit(DebugEvent{Phase: "search-unknown", Group: g.Name, QueryName: q.Name, Note: "unknown search type: " + q.Type})
			}
		}
	}

	// Optional: verify code file recency by hitting commits endpoint for each file
	if cfg.UseCommitCheck && len(codeHits) > 0 {
		emit(DebugEvent{Phase: "commit-check", Note: fmt.Sprintf("files=%d", len(codeHits))})
		codeHits = enrichWithCommitDates(ctx, client, since, codeHits)
		// keep only those with commitDate >= since; drop unverified
		out := codeHits[:0]
		for _, h := range codeHits {
			if h.CommitDate.IsZero() {
				continue
			}
			if !h.CommitDate.Before(since) {
				out = append(out, h)
			}
		}
		codeHits = out
		emit(DebugEvent{Phase: "commit-check-done", Note: fmt.Sprintf("kept=%d", len(codeHits))})
	}

	// Ensure deterministic ordering by recency
	if len(codeHits) > 1 {
		sort.Slice(codeHits, func(i, j int) bool {
			ai := codeHits[i].CommitDate
			aj := codeHits[j].CommitDate
			if ai.IsZero() && aj.IsZero() { return false }
			if ai.IsZero() { return false }
			if aj.IsZero() { return true }
			return ai.After(aj)
		})
	}
	if len(repoHits) > 1 {
		sort.Slice(repoHits, func(i, j int) bool { return repoHits[i].PushedAt.After(repoHits[j].PushedAt) })
	}

	return Findings{
		RunID:    "", // filled by caller
		SinceISO:  sinceISO,
		DaysBack:  cfg.DaysBack,
		Generated: time.Now().Format(time.RFC3339),
		CodeHits:  dedupeCode(codeHits),
		RepoHits:  dedupeRepo(repoHits),
		Notes:     notes,
	}, nil
}

func enrichWithCommitDates(ctx context.Context, c *ghClient, since time.Time, hits []CodeHit) []CodeHit {
	type job struct{ i int; h CodeHit }
	type res struct{ i int; t time.Time }

	jobs := make(chan job)
	results := make(chan res)
	wg := sync.WaitGroup{}

	worker := func() {
		defer wg.Done()
		for j := range jobs {
			ownerRepo := j.h.Repository
			path := j.h.FilePath
			url := fmt.Sprintf("https://api.github.com/repos/%s/commits?path=%s&since=%s&per_page=1",
				ownerRepo, neturl.PathEscape(path), since.Format(time.RFC3339))
			reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			resp, err := c.get(reqCtx, url)
			cancel()
			if err != nil {
				results <- res{j.i, time.Time{}}
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			throttleFrom(resp)
			var cr commitResp
			if resp.StatusCode == 200 {
				_ = json.Unmarshal(body, &cr)
				if len(cr) > 0 {
					d, _ := time.Parse(time.RFC3339, cr[0].Commit.Author.Date)
					results <- res{j.i, d}
					continue
				}
			}
			results <- res{j.i, time.Time{}}
		}
	}

	wg.Add(maxConcurrentDetails)
	for k := 0; k < maxConcurrentDetails; k++ {
		go worker()
	}
	go func() {
		for i, h := range hits {
			jobs <- job{i, h}
		}
		close(jobs)
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]CodeHit, len(hits))
	copy(out, hits)
	for r := range results {
		out[r.i].CommitDate = r.t
	}
	return out
}

func dedupeCode(in []CodeHit) []CodeHit {
	seen := map[string]bool{}
	out := make([]CodeHit, 0, len(in))
	for _, h := range in {
		key := h.Repository + "|" + h.FilePath + "|" + h.FileURL
		if !seen[key] {
			seen[key] = true
			out = append(out, h)
		}
	}
	return out
}

func dedupeRepo(in []RepoHit) []RepoHit {
	seen := map[string]bool{}
	out := make([]RepoHit, 0, len(in))
	for _, h := range in {
		key := h.FullName
		if !seen[key] {
			seen[key] = true
			out = append(out, h)
		}
	}
	return out
}

func urlQueryEscape(q string) string {
	// Encode for query param but preserve GitHub search operators so semantics remain intact.
	// Start with strict escaping, then unescape a safe subset used by GitHub search: :, (), >, <, =, ,, /, |
	enc := neturl.QueryEscape(strings.TrimSpace(q))
	enc = strings.ReplaceAll(enc, "%3A", ":")
	enc = strings.ReplaceAll(enc, "%3a", ":")
	enc = strings.ReplaceAll(enc, "%28", "(")
	enc = strings.ReplaceAll(enc, "%29", ")")
	enc = strings.ReplaceAll(enc, "%3E", ">")
	enc = strings.ReplaceAll(enc, "%3e", ">")
	enc = strings.ReplaceAll(enc, "%3C", "<")
	enc = strings.ReplaceAll(enc, "%3c", "<")
	enc = strings.ReplaceAll(enc, "%3D", "=")
	enc = strings.ReplaceAll(enc, "%3d", "=")
	enc = strings.ReplaceAll(enc, "%2C", ",")
	enc = strings.ReplaceAll(enc, "%2c", ",")
	enc = strings.ReplaceAll(enc, "%2F", "/")
	enc = strings.ReplaceAll(enc, "%2f", "/")
	enc = strings.ReplaceAll(enc, "%7C", "|")
	enc = strings.ReplaceAll(enc, "%7c", "|")
	return enc
}
func urlPathEscape(p string) string {
	return neturl.PathEscape(p)
}

var forkQual = regexp.MustCompile(`(?i)\bfork\s*:\s*(true|false|only)\b`)
func sanitizeCodeQuery(q string) string {
	q = forkQual.ReplaceAllString(q, "")
	q = strings.Join(strings.Fields(q), " ")
	return strings.TrimSpace(q)
}

func throttleFrom(resp *http.Response) {
    resource := strings.ToLower(resp.Header.Get("X-RateLimit-Resource"))
    var baseWait time.Duration
    if resource == "search" {
        baseWait = 12 * time.Second // ~5 req/min for search endpoints
    } else {
        baseWait = 1500 * time.Millisecond
    }

    rem, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))

    // Honor Retry-After when sent (secondary rate limits)
    if ra := resp.Header.Get("Retry-After"); ra != "" {
        if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
            time.Sleep(time.Duration(secs)*time.Second + 500*time.Millisecond)
            return
        }
    }

    // If rate limited or nearly there, wait until reset (with caps)
    if resp.StatusCode == 403 || rem <= 2 {
        if resetUnix, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
            wait := time.Until(time.Unix(resetUnix, 0))
            if wait > 0 {
                capWait := 2 * time.Minute
                if resource != "search" {
                    capWait = 5 * time.Minute
                }
                if wait > capWait { wait = capWait }
                time.Sleep(wait + 500*time.Millisecond)
                return
            }
        }
        // Fallback conservative backoff if no usable reset
        if resource == "search" {
            time.Sleep(90 * time.Second)
        } else {
            time.Sleep(30 * time.Second)
        }
        return
    }

    // Normal gentle pacing + jitter
    jitterMs := time.Now().UnixNano() % int64(2000*time.Millisecond)
    time.Sleep(baseWait + time.Duration(jitterMs))
}

// ====== OpenAI drafting ======

func draftReportWithOpenAI(ctx context.Context, cfg AppSettings, f Findings) (string, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return "", errors.New("OPENAI_API_KEY missing")
	}

	// Keep payload compact to fit token limits
	type smallCode struct {
		Repo string `json:"repo"`
		URL  string `json:"url"`
		Path string `json:"path"`
		Lang string `json:"lang"`
		Commit string `json:"commit,omitempty"`
	}
	type smallRepo struct {
		Full string `json:"full"`
		URL  string `json:"url"`
		Desc string `json:"desc,omitempty"`
		Pushed string `json:"pushed"`
	}

	codes := make([]smallCode, 0, min(200, len(f.CodeHits)))
	for i, h := range f.CodeHits {
		if i >= 200 { break }
		c := smallCode{
			Repo: h.Repository, URL: h.FileURL, Path: h.FilePath, Lang: h.Language,
		}
		if !h.CommitDate.IsZero() {
			c.Commit = h.CommitDate.Format("2006-01-02")
		}
		codes = append(codes, c)
	}
	repos := make([]smallRepo, 0, min(200, len(f.RepoHits)))
	for i, h := range f.RepoHits {
		if i >= 200 { break }
		repos = append(repos, smallRepo{
			Full: h.FullName, URL: h.HTMLURL, Desc: h.Description, Pushed: h.PushedAt.Format("2006-01-02"),
		})
	}

	raw := map[string]any{
		"since": f.SinceISO,
		"daysBack": f.DaysBack,
		"codeHits": codes,
		"repoHits": repos,
		"notes": f.Notes,
	}
	rawJSON, _ := json.Marshal(raw)

	sys := "You are an assistant that writes concise, developer-friendly Markdown reports. " +
		"Summarize GitHub search findings that touch market-data/broker APIs (Polygon.io, Alpaca, IBKR, Databento). " +
		"Group by API when obvious (infer from URLs or package names), then list notable repos/files as bullet points with links. " +
		"Prefer code hits over repo mentions. Include a short 'What to study' checklist (rate limiting, auth, streaming/REST). " +
		"Do not invent content; only use provided JSON. If there are zero results and no explicit error message in notes, say 'No results found in the selected window' and do not guess about parsing errors or rate limits."

	usr := "Create a Markdown report for findings in the last " + strconv.Itoa(f.DaysBack) + " days.\n" +
		"Raw findings JSON:\n```\n" + string(rawJSON) + "\n```"

	payload := map[string]any{
		"model": cfg.OpenAIModel,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": usr},
		},
	}

	reqBody, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", errors.New("no choices from OpenAI")
	}
	return out.Choices[0].Message.Content, nil
}

func min(a, b int) int {
	if a < b { return a }
	return b
}

func max(a, b int) int {
	if a > b { return a }
	return b
}

// ====== helpers ======

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n { return s }
	return s[:n] + "…"
}

func buildFallbackMarkdown(f Findings, err error) string {
	var b strings.Builder
	b.WriteString("# Report (fallback)\n\n")
	if err != nil {
		b.WriteString("OpenAI drafting failed: ")
		b.WriteString(err.Error())
		b.WriteString("\n\n")
	}
	b.WriteString("Window: ")
	b.WriteString(f.SinceISO)
	b.WriteString(" (last ")
	b.WriteString(strconv.Itoa(f.DaysBack))
	b.WriteString(" days)\n\n")
	b.WriteString("- Code hits: ")
	b.WriteString(strconv.Itoa(len(f.CodeHits)))
	b.WriteString("\n")
	b.WriteString("- Repo hits: ")
	b.WriteString(strconv.Itoa(len(f.RepoHits)))
	b.WriteString("\n\n")
	if len(f.Notes) > 0 {
		b.WriteString("Notes:\n")
		for _, n := range f.Notes {
			b.WriteString("- ")
			b.WriteString(n)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	maxList := min(10, len(f.CodeHits))
	if maxList > 0 {
		b.WriteString("Top code hits:\n")
		for i := 0; i < maxList; i++ {
			h := f.CodeHits[i]
			b.WriteString("- ")
			b.WriteString(h.Repository)
			b.WriteString(": ")
			b.WriteString(h.FileURL)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	maxRepos := min(10, len(f.RepoHits))
	if maxRepos > 0 {
		b.WriteString("Top repos:\n")
		for i := 0; i < maxRepos; i++ {
			r := f.RepoHits[i]
			b.WriteString("- ")
			b.WriteString(r.FullName)
			b.WriteString(": ")
			b.WriteString(r.HTMLURL)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("See diagnostics: /api/last-raw\n")
	return b.String()
}

func (s *Server) emitFunc(runID string) func(DebugEvent) {
	return func(ev DebugEvent) {
		ev.TS = time.Now().Format(time.RFC3339)
		ev.RunID = runID
		s.runsMu.Lock()
		s.runs[runID] = append(s.runs[runID], ev)
		if len(s.runs[runID]) > 1000 {
			s.runs[runID] = s.runs[runID][len(s.runs[runID])-1000:]
		}
		s.runsMu.Unlock()
		log.Printf("[%s] %s %s %s (page=%d status=%d rl=%s rs=%s) %s",
			ev.RunID, ev.TS, ev.Phase, ev.QueryName, ev.Page, ev.Status, ev.RateRemaining, ev.RateReset, ev.Note)
	}
}

func newRunID() string {
	return time.Now().UTC().Format("20060102T150405Z")
}

// ====== Default queries.yaml ======

const defaultQueriesYAML = `# queries.yaml
# You can edit this file from the UI. Toggle 'enabled' to include/exclude groups or searches.
# Tip: keep queries tight and language-specific when possible; 'sort=indexed' is applied automatically for code.

groups:
  - name: Polygon.io
    enabled: true
    searches:
      - name: Polygon REST endpoints
        type: code
        enabled: true
        query: "\"api.polygon.io\""
      - name: Polygon Python usage
        type: code
        enabled: true
        query: "\"import polygon\" OR \"from polygon\" language:python"
      - name: Go module
        type: code
        enabled: true
        query: "filename:go.mod \"github.com/polygon-io\""
      - name: Repo mention (README/desc)
        type: repo
        enabled: true
        query: "(polygon OR \"api.polygon.io\") in:readme,description"

  - name: Alpaca
    enabled: true
    searches:
      - name: Alpaca REST endpoints
        type: code
        enabled: true
        query: "\"api.alpaca.markets\" OR \"paper-api.alpaca.markets\""
      - name: Alpaca Python client
        type: code
        enabled: true
        query: "\"import alpaca_trade_api\" language:python"
      - name: Go client
        type: code
        enabled: true
        query: "\"github.com/alpacahq/alpaca-trade-api-go\" language:go"
      - name: Repo mention
        type: repo
        enabled: true
        query: "(alpaca OR \"alpaca.markets\") in:readme,description"

  - name: IBKR
    enabled: true
    searches:
      - name: ibapi / ib_insync (Python)
        type: code
        enabled: true
        query: "\"import ibapi\" OR \"from ibapi\" OR \"import ib_insync\" OR \"from ib_insync\" language:python"
      - name: Java client classes
        type: code
        enabled: true
        query: "\"com.ib.client\" language:java"
      - name: Repo mention
        type: repo
        enabled: true
        query: "(ibkr OR ibapi OR \"Interactive Brokers\") in:readme,description"

  - name: Databento
    enabled: true
    searches:
      - name: Databento Python
        type: code
        enabled: true
        query: "\"import databento\" OR \"from databento\" language:python"
      - name: Endpoints / hostnames
        type: code
        enabled: true
        query: "\"hist.databento.com\" OR \"live.databento.com\""
      - name: Repo mention
        type: repo
        enabled: true
        query: "databento in:readme,description"
`
