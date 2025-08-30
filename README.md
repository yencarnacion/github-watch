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
