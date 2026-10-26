# Upstream contract fixtures

Synthetic, credential-free examples based on caozhiyuan/copilot-api release
`v2.7.38`, commit `1236d73c1e1692d659e590bd99190a8c1d697c38`, inspected on
2026-10-24:

- `src/services/github/get-copilot-usage.ts`
- `src/routes/token-usage/route.ts`
- `src/lib/token-usage/store.ts`

These fixtures verify decoding and field mapping. They are not captured live
responses and do not establish deployment acceptance. The examples use the
native `byModel`, `request_count`, nullable AIU, integer cost nanos, and pagination
fields. Model entries include nullable average TTFT and output speed. Event
entries include request ID, TTFT, duration, outcome, output speed, and User-Agent.
The Dashboard currently decodes duration and outcome for display; the other new
fields are ignored. Missing or null timing remains unavailable, rather than zero.
Unknown upstream fields are ignored; missing account identity or required dataset
structure is rejected.

Quota calculations follow Microsoft VS Code's `getQuotaUsage` at commit
`3ac0f0563e382d8e3480c787d04a2d05afca6746`, inspected on 2026-10-26, in
`src/vs/workbench/services/chat/common/chatEntitlementService.ts`. The client
accepts numeric or numeric-string entitlements and preserves `has_quota`.
Frontend tests cover independent credits, fractional remaining quotas,
percentage-only snapshots, zero entitlement, and unavailable pooled quotas.
