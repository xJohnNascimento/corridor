# ADR 0007: Human decisions are durable commands delivered as Updates

Status: accepted (2026-11-02)

**Context.** Some outcomes need people: a fill beyond tolerance, or a payout the provider cannot confirm. Decisions must survive crashes, never apply twice, and be attributable to two distinct people.

**Decision.** Customers and operators act through commands stored in Postgres before delivery. The dispatcher delivers each command as an Update whose id is the command id. Validators check shape only. The handler calls one activity that decides against durable state, stores ACCEPTED or REJECTED and returns; it never waits on the workflow's progress. The main loop applies the effect. Every review reason has at least one decision, including retrying a step after a correction and resuming a stopped payment. Proposals bind their exact evidence; an approval names the proposal's digest and must come from another operator. The API is the authentication authority, and credentials never enter workflow history.

**Consequences.** No decision is lost or duplicated by an API or dispatcher crash. Refused attempts are recorded. History holds no credentials. A compromised API could impersonate an operator; the threat model says so.

**Alternatives considered.** Verifying operator tokens inside Update validators (key caches, freshness rules and I/O in validators); Signals for decisions (no validation and no answer).
