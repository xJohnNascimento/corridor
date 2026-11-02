# Temporal spike

The behaviours this design relies on, checked against a real Temporal server before any workflow is written. `make spike` re-runs them; CI runs them on every pull request, so an SDK or server upgrade that changes one fails the build.

**Last run:** 2026-11-02, Temporal Go SDK v1.50.0, Temporal CLI dev server 1.9.1 (`temporalio/temporal:1.9.1`), Go 1.27.3. Source: [`test/spike/spike_test.go`](../test/spike/spike_test.go).

| Behaviour | Where the design relies on it | Result |
| --- | --- | --- |
| Update-With-Start returns the handler's answer as soon as the workflow decides; the same Update id re-attaches to the same run; with no decision, the handler's timer bound returns `ADMITTED` | Admission fast path ([design, section 5](design.md#5-admission)) | Pass: decided in under 2 s; same run on retry; `ADMITTED` at the 2 s bound |
| A validator rejection leaves no Update event in history | Command validators check shape only ([7.2](design.md#72-signals-updates-and-handlers)) | Pass: 0 accepted events after a rejection, 1 after a valid Update |
| Activity retries add no history events | `PollPayout` retries are the polling loop ([7.1](design.md#71-activities)) | Pass: 5 attempts, one scheduled, one started and one completed event |
| `GetContinueAsNewSuggested` turns true at the server's configured history limit | Continue-As-New at safe boundaries ([7.3](design.md#73-long-histories)) | Pass, with the limit set to 50 events |
| `TaskQueueActivitiesPerSecond` is enforced by the server across workers | Load shaping on `fx-provider` ([13.2](design.md#132-capacity)) | Pass: two workers each set to 2/s ran 12 activities at 1.0/s in total |
| `AllHandlersFinished` lets an in-flight Update finish before the workflow completes | `finish` waits for handlers ([7.2](design.md#72-signals-updates-and-handlers)) | Pass |
| A cancelled workflow can run a cleanup activity on a disconnected context | `MarkRecoveryRequired` ([7.4](design.md#74-bugs-cancellation-and-termination)) | Pass: cleanup ran once; the workflow ended cancelled |
| `REJECT_DUPLICATE` refuses a closed id; `USE_EXISTING` returns the running run; `ALLOW_DUPLICATE_FAILED_ONLY` allows a new run after termination and refuses one after completion | Dispatcher starts and RESUME ([9](design.md#9-webhook-inbox-and-dispatcher)) | Pass |

## Observations that change numbers, not the design

- **The task-queue activity rate came out below its setting.** Two workers set to 2/s produced 1.0/s in total. The capacity estimate in [section 13.2](design.md#132-capacity) must be measured under load, not derived from the configured rate.
- **Timers and retry backoffs fire with about one-second granularity on the dev server.** A 50 ms retry interval and 1 ms timers each took about a second. The demo policy (2 s fast-path bound, 2 s first poll) is compatible; nothing may depend on sub-second timing.
