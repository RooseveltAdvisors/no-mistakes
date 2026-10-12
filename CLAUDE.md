<!-- Points Claude at AGENTS.md via import; edit AGENTS.md, not this file. -->
@AGENTS.md

<!-- CODE-INTEL-ROUTING:START (managed by code-index.sh) -->
## Code-intelligence routing (this repo is indexed)
This repo has CodeGraph + codebase-memory-mcp indexes. **The agent self-routes by the question — never asks which tool:**
- call-flow / "how does X reach Y" / blast-radius before a change → **CodeGraph** (`codegraph_explore`, `impact`)
- worst functions / hotspots / dead code / complexity / architecture → **codebase-memory-mcp** (Cypher on `f.complexity`, `get_architecture`)
- an exact string, a file you know, a tiny/config/bash file, or freshness → **grep** (the floor)
- **Hybrid:** cbm finds the hotspot → CodeGraph scopes the blast radius → read the source to confirm.
Reindex is automatic (CodeGraph auto-syncs on save). Full governance: the `retrieval-routing-governance` rule.
<!-- CODE-INTEL-ROUTING:END -->
