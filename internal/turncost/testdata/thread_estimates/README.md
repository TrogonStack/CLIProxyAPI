# Thread estimates fixtures

Responses captured from `POST /backend-api/wham/usage/thread-estimates/query` on 2026-11-07
and sanitized: thread, turn, and response ids are fake. `spec_test.go` serves each file from
an httptest server and asserts how the proxy reacts, so this directory is the spec of the
upstream behavior the pass-through relies on.

Each file has `case`, `request`, `status`, `content_type`, and `response` (JSON) or
`response_raw` (text). `native_usage_usd_micros` appears in real responses and is decoded but unused.

| case | status | upstream behavior | proxy outcome |
| --- | --- | --- | --- |
| business_real_turns | 200 | five turns with USD micros, model, and settled response ids | priced from upstream |
| business_without_settled_ids | 200 | `include_settled_response_ids` false: `settled_response_ids` key absent | not accepted, local fallback |
| business_unknown_turn | 200 | every field null, settled ids empty | local fallback, never $0, nothing cached |
| business_missing_account_header | 200 | same as unknown turn when the account header is missing | local fallback, nothing cached |
| business_mixed_known_unknown | 200 | five priced turns and one null turn | accepted per turn, null turn falls back |
| business_empty_threads | 400 | `{"error":{...}}` for an empty thread list | client bug: local fallback, nothing cached |
| business_malformed_body | 400 | `{"error":{...}}` for a malformed body | client bug: local fallback, nothing cached |
| prolite_forbidden | 403 | `{"detail":"Forbidden"}` | account marked forbidden, local fallback |
| free_forbidden | 403 | `{"detail":"Forbidden"}` | account marked forbidden, local fallback |
| invalid_token | 401 | `{"detail":"Could not parse your authentication token..."}` | local fallback, nothing cached |

A plan type does not predict access (a business-family prolite plan also gets 403), so an
account is probed by its first real query and the answer is remembered.
