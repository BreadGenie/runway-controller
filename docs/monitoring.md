# Runway monitoring

Set `metrics_listen` in the controller or standalone cache-data JSON to a
loopback address such as `127.0.0.1:18446`. Managed cache uses `metrics_listen`
for the cache-service translator and `data_metrics_listen` for cache-data.
Atlas uses ports 18446 and 18447 on the cache VM. Omit the fields to disable
the listeners. Each serves Prometheus text at `GET /metrics`.

| Metric | Meaning |
| --- | --- |
| `runway_controller_assigned_jobs`, `runway_controller_waiting_jobs`, `runway_controller_running_runners` | GitHub's assigned jobs, assigned minus running jobs, and Runner Leases in the running phase. A job offer also signals waiting demand before statistics catch up. |
| `runway_controller_unserved_seconds` | Time since the controller first observed the current uninterrupted period with waiting jobs. GitHub statistics provide counts, so this is an observed backlog age. |
| `runway_controller_no_progress_seconds` | Time with waiting demand and no executing job. An executing job occupies the pilot's only slot and clears this timer. |
| `runway_controller_ownership_fence`, `runway_controller_ownership_renewal_age_seconds`, `runway_controller_ownership_renewal_timestamp_seconds` | Current Central fence and time since the last successful claim or renew RPC started. |
| `runway_controller_cleanup_leases`, `runway_controller_cleanup_oldest_seconds` | Number and maximum age of leases awaiting VM or GitHub removal. Age starts at first observation of cleanup or at an earlier known lease deadline. Early cleanup age before a controller restart may be unavailable. |
| `runway_cache_hits_total`, `runway_cache_misses_total` | Authorized cache-data lookups that issued a download ticket or returned a miss. |
| `runway_cache_routed_total`, `runway_cache_fallback_total` | Managed guest cache action invocations routed to Runway or left on GitHub, including allowlist refusals. Calculate the fallback ratio as fallback growth divided by the sum of routed and fallback growth. |
| `runway_cache_translator_errors_total`, `runway_cache_relay_errors_total` | Failed requests at the managed translator, including missing sessions, and guest relay connection or I/O errors and connection-limit refusals. |
| `runway_cache_filesystem_available_bytes`, `runway_cache_filesystem_size_bytes` | Free bytes available to the service and total bytes for the journal and spool filesystems. |
| `runway_cache_extraction_available_bytes` | Lowest observed free bytes across the active guest workspace and runner temporary directory, when available. |

Controller metrics use only Runner Pool and region labels. Guest observations
require the updated guest agent, transport and baked cache helpers. They are
best effort, gathered during status and cleanup, and the job can influence
them. Counters reset on exporter restart. A guest destroyed before its next
observation can lose its latest events.

## Trace controller assignment and verification

The controller writes these JSON events to stderr with a UTC `timestamp` and
the Runner Pool. Join delivery events on `message_id` within the same session.
Join later activity on `lease_id`, runner name and ID, `run_id`, and `job_uuid`
where present. Verification uses `attempt` for the controller's retry number.

| Event | What to compare |
| --- | --- |
| `controller_scale_set_received` | Logged by the listener before handing off the message. Compare the matching job's GitHub start or finish timestamp to this timestamp for time to controller receipt. |
| `controller_scale_set_handling` | Receipt to handling is time waiting for the serialized engine. |
| `controller_job_evidence_written` | Handling to `assigned`, `started`, or `completed` includes ownership refresh and work before the durable write. Replays that skip a write have no evidence event. |
| `controller_scale_set_finished` | Handling to finish covers ownership refresh and synchronous processing. The outcome precedes SDK acknowledgment. |
| `controller_assignment_hint_created`, `controller_assignment_hint_resolved` | Creation to resolution covers waiting for an eligible runner, REST attempts and retries, and the durable assignment write. |
| `controller_assignment_hint_rejected`, `controller_assignment_hint_expired`, `controller_assignment_hint_exhausted` | Fixed reasons distinguish cases such as `ambiguous_name`, `runner_or_job_mismatch`, `job_not_running`, TTL expiry, and retry exhaustion. REST can report no eligible running job without distinguishing an invisible job from one on a different runner. |
| `controller_verification_scheduled`, `controller_verification_rest_result` | Evidence to scheduling is the wait for the verifier to become eligible and available. Scheduling to REST result covers worker dispatch and GitHub REST calls. |
| `controller_verification_applied`, `controller_verification_discarded` | REST result to disposition covers the wait for the engine to consume the result and, if applied, persist it. An applied result can record a failed verification attempt. Discard reasons include `lease_retiring`, `lease_expired`, `pool_disabled`, `stale_identity`, and `write_failed`. |
| `controller_verification_skipped` | `started_and_completed` marks a message that retired the same job's lease before verification could be scheduled from its start evidence. |

Assignment and verification workers wake the serialized engine when their results are ready. Durable assignment can schedule verification in the same engine turn. The verifier still reads repository policy, the run, and its attempt-specific jobs independently. A result wake refreshes ownership before making a fenced write. The periodic reconcile still drives retries.

## Send metrics to Atlas

Use the local Vector instance on the controller host, including when that
host is operator-managed. Use a regional Datum write token scoped to a stable,
unique controller resource ID. Keep the token in the Vector service
environment. Configure a local collector for the managed cache VM separately.

Set `metrics_listen` in the controller host's cache-reconcile configuration
to a loopback address such as `127.0.0.1:18448` to scrape `/metrics`.

| Metric | Meaning |
|---|---|
| `runway_cache_reconcile_current_vm` | The journalled VM for the region, including while its deletion is pending. |
| `runway_cache_reconcile_attempts_total` | Create attempts recorded before Atlas submission. |
| `runway_cache_reconcile_replacements_total` | Replacement sequences started by the reconciler. |
| `runway_cache_reconcile_last_error` | Error class from the latest reconciliation for the region, or `none` after a successful pass. |

```yaml
sources:
  runway_controller:
    type: prometheus_scrape
    endpoints: ["http://127.0.0.1:18446/metrics"]
    scrape_interval_secs: 30
sinks:
  datum_runway:
    type: prometheus_remote_write
    inputs: [runway_controller]
    endpoint: "${DATUM_URL}/v1/ingest/remote"
    auth:
      strategy: bearer
      token: "${DATUM_TOKEN}"
    healthcheck:
      enabled: false
    batch:
      timeout_secs: 30
```

Use the existing Datum sink if the host already has one. Import the updated
Atlas Insights workbook with the Raven webhook during the production trial.

| Schedule | Alert |
| --- | --- |
| P1, every 5 minutes | Central renewal age at least 60 seconds, unserved demand without progress for at least 5 minutes, or journal/spool disk below 10% available. |
| P2, hourly | Cleanup at least 10 minutes old, fallback above 25% with at least 5 decisions in the last hour, or journal/spool disk below 25% available. |

The ownership check uses the newest renewal timestamp seen for a pool over
the last hour. It can alert when scrapes stop during that window. It needs at
least one successful scrape to observe a controller. The demand and disk
checks use recent samples. Existing Atlas `system_*` filesystem and ZFS checks
cover Metal host image and VM storage.
