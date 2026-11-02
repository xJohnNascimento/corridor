# ADR 0008: No personal data in workflow history

Status: accepted (2026-11-02)

**Context.** Temporal histories are retained, replicated and visible in the UI. Beneficiary details and credentials are sensitive.

**Decision.** Workflow inputs, Update arguments and Signals carry ids, amounts and the policy snapshot only. Activities load beneficiary details from Postgres when they call the provider. The first release has no payload codec.

**Consequences.** Histories can be exported as evidence after light sanitizing. There is no codec key to manage. Activities make one extra read.

**Alternatives considered.** An AES-GCM payload codec with a codec server (needed if sensitive data must enter history; adds key rotation and decoding in the UI).
