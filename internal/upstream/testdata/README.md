# Upstream contract fixtures

Synthetic, credential-free examples based on caozhiyuan/copilot-api commit
`0e419052cdce5f949fc656d715a9ccc3d2abb743`, inspected on 2026-09-08:

- `src/services/github/get-copilot-usage.ts`
- `src/routes/token-usage/route.ts`
- `src/lib/token-usage/store.ts`

These fixtures verify decoding and field mapping. They are not captured live
responses and do not establish deployment acceptance. The examples use the
native `byModel`, `request_count`, nullable AIU, integer cost nanos, and pagination
fields. Unknown upstream fields are ignored; missing account identity or required
dataset structure is rejected.
