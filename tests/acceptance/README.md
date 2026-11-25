# Independent HTTP/MCP acceptance

`TestPublicRESTAndMCP` uses only public HTTP and MCP Streamable HTTP interfaces. It does not import server packages or open a database connection. Run the Go test process outside the backend container with `PAM_ACCEPTANCE_URL` and `PAM_ACCESS_TOKEN` set; without both variables it explicitly skips.

The test records synthetic request and response bodies through `t.Log`. It never logs the access token. Capture verbose test output as the acceptance evidence. The fixture uses a unique `pam-acceptance-...` source, an unconfirmed Synthetic Mall / Synthetic Gadget purchase candidate, and a decimal value larger than IEEE-754's exact integer range.

Coverage includes authenticated REST/MCP, original source traceability, exact money, idempotent replay and changed-key conflicts, external IDs, ambiguous fingerprints and explicitly confirmed repeated purchases, all transaction filters and pagination, retained invalid imports through the MCP import tool, inventory CRUD and foreign-key validation, MCP discovery, schema resources, arbitrary read-only candidate/correlation queries, rejected SQL and runtime SQL errors, audit terminal states, original capture and normalized-pattern privacy, equivalent-query fingerprints and frequency summaries, and query row truncation.

The test deletes only assets that it created. Imported transactions, import provenance, and query audit are deliberately retained under the logged synthetic source because no public generic-delete operation exists. Use an isolated acceptance database. Archive the evidence before any explicitly scoped database cleanup. The test never clears tables, deletes another source, or asserts global production row counts.

This establishes protocol behavior from an independent application client. It does not establish an authenticated ChatGPT connector session, HTTPS ingress behavior, an iOS simulator run, or physical-device acceptance.
