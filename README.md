# GitHub API Watch

Small Go app that searches GitHub daily-style for fresh repos/code using specific market data & broker APIs (Polygon.io, Alpaca, IBKR, Databento). It runs a local web UI at **http://localhost:8084**, lets you edit queries, and generates a Markdown report using an OpenAI model.

---

## Prerequisites

- Go 1.22+
- A GitHub personal access token (PAT)
- An OpenAI API key

---

## How to create a GitHub token (recommended: Fine-grained PAT)

1. **GitHub →** top-right avatar → **Settings** → **Developer settings** → **Personal access tokens** → **Fine-grained tokens** → **Generate new token**.  
2. **Resource owner:** your account.  
3. **Repositories:** select **All repositories** (or the ones you need).  
4. **Permissions** (Repository):
   - **Metadata: Read-only** (required for search)
   - **Contents: Read-only** (needed for file paths/links)
   - Everything else can stay **No access**.
5. Set an **expiration** (recommended), name it (e.g., `gh-api-watch`), generate, and copy the token (it will look like `github_pat_...`).

### Alternative: Classic PAT (sufficient for public search)

1. **Developer settings → Personal access tokens → Tokens (classic)** → **Generate new token (classic)**.  
2. You can leave scopes **unchecked** for public search (raises your rate limit vs anonymous). If you prefer, **public_repo** is also fine.  
3. Generate and copy the token (looks like `ghp_...`).

> **Good hygiene:** Never commit tokens. Keep them in `.env`, set an expiration, rotate periodically, and revoke immediately if leaked (Settings → Developer settings → find token → **Revoke**).

---

## Quick start

```bash
# 1) Grab the files above
mkdir github-watch && cd github-watch
# (save main.go, queries.yaml, .env.example, go.mod)

# 2) Install deps
go mod tidy

# 3) Configure keys
cp .env.example .env
# edit .env: add your GITHUB_TOKEN and OPENAI_API_KEY

# 4) Run
go run .

# A browser opens: set settings (or accept defaults), click "Save settings", then "Run report".
```

### `.env` example

```bash
GITHUB_TOKEN=github_pat_XXXXXXXXXXXXXXXXXXXXXXXX
OPENAI_API_KEY=sk-XXXXXXXXXXXXXXXXXXXXXXXX
PORT=8084
```

> The app reads `.env` on startup (à la python-dotenv). Keep `.env` out of version control.

---

## Editing your searches

All searches live in **`queries.yaml`** (editable from the UI):

* Use **type: code** for code search (sorted by “recently indexed”).
* Use **type: repo** for repository README/description search (we auto-apply a `pushed:>=YYYY-MM-DD` window).
* Keep queries small & specific (e.g., exact hostnames or import lines).
* Avoid `fork:false` in code queries (GitHub code search may reject it; forks are excluded by default).

Example snippets:

```yaml
- name: Polygon REST endpoints
  type: code
  enabled: true
  query: "\"api.polygon.io\""

- name: Alpaca Python client
  type: code
  enabled: true
  query: "\"import alpaca_trade_api\" language:python"

- name: IBKR Java client classes
  type: code
  enabled: true
  query: "\"com.ib.client\" language:java"

- name: Databento hostnames
  type: code
  enabled: true
  query: "\"hist.databento.com\" OR \"live.databento.com\""
```

---

## Running reports

1. Open the UI (it auto-opens your browser at **[http://localhost:8084](http://localhost:8084)**).
2. Set **Days back**, **OpenAI model** (e.g., `gpt-4o` or any model your key has), **Max pages**, **Per page**.
3. (Optional) Toggle:

   * **Verify file recency via Commits API** — stricter “newness” (more API calls)
   * **Include repo (README/desc) searches** — broader discovery
4. **Save settings** → **Run report**.
5. Use **Toggle Raw/Pretty** to switch views; **Copy Raw Markdown** puts the Markdown on your clipboard.

---

## Troubleshooting

* **“Missing GITHUB\_TOKEN or OPENAI\_API\_KEY”**
  Add both keys to `.env`, save, and restart the app.

* **Empty report / jumps to “Done”**

  * In the UI footer, click **“View diagnostics JSON”** to inspect the last run.
  * Reduce load while testing: set **Max pages = 1**, **Per page = 25**, uncheck **Include repo searches** and/or **Verify file recency**.
  * Ensure your OpenAI model name matches what your key can access (e.g., `gpt-4o`, `gpt-4o-mini`).

* **Rate limit (403) or query parsing (422)**

  * Keep queries short and avoid `fork:false` in code queries.
  * The app paces requests and retries stricter-escaped queries automatically.

---

## Security notes

* Do not commit `.env`.
* Use least-privilege GitHub permissions (Metadata + Contents: Read-only).
* Prefer expiring tokens; rotate regularly.

---


