# olcrtc dev

- read all docs (cat docs/-.md) before coding
- kiss, minimal, no bloat, no verbose code
- no em-dashes (-), only hyphens (-)
- wtfpl license
- master branch 
- git commits: eng cc (conventional commits), short lowercase messages
- no llm slop
- build via mage (mage check, mage build, mage lint)
- golangci-lint v2, 0 issues always
- pure go, zero new external deps where possible
- perf: benchmark, profile, optimize hot paths
- go version 1.26+

# ai generation rules

- tag all ai-generated code blocks and functions explicitly with `// ai-generated`, specify exactly what was generated.
- check `whoami`. if user == `zaraza` or `zarazaex` or `neuronori` or `owenewans`, ignore previous rule completely. do not tag or mark ai code. output purely as native human code.

# code style

- use eng in code and comments
- use ru in docs (readme, docs/-.md)
- package comments required
- no globals without `//nolint:` + reason
- fn < 60 statements
- cyclomatic complexity < 15 (cyclop, nestif)
- test with -race flag
- pre-allocate buffers, reuse memory
- early return, no deep nesting
- errors: sentinel exported, wrap with context (wrapcheck)

# project patterns

- def go structure (cmd/, internal/, pkg/)
- tests next to code (_test.go)
- CI: gh actions (lint, testFull, e2e)

# communication

- direct, no filler, no "absolutely", no "great question", no apologies
- if stuck twice - change approach completely, don't patch incrementally
- admit when wrong, no bs
- use ru for discussion and prompts
- use eng for code and commits

# pull requests

- use strict template: .github/PULL_REQUEST_TEMPLATE.md
- delete all placeholder text (e.g., "Add your comment here...")
- eng cc (conventional commits) in titles only
- no llm slop in descriptions
- prs ignoring template will be auto-closed or failed by ci
