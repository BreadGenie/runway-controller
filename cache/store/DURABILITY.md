# Cache journal durability

The private state directory is protected by flock. State operations use an opened
directory descriptor, reject symlinks and require atomic rename and fsync.
The journal stores namespace, storage identity, entry IDs, reservation sizes,
object keys, digests, timestamps, publication/deletion intents and ticket hashes.
It stores no bearer tokens or storage credentials.

Reservations are journaled before acknowledgement. Finalization records its
object intent before PUT, and publication before returning success.
Deletion records hidden intent before DELETE, releasing accounting only after
success. Journal writes use private temporary files, file fsync, rename and
directory fsync. A journal failure fences storage effects until restart.

Startup verifies every indexed published object against its size and SHA256
within one shared size-scaled recovery budget. A missing or damaged object
becomes a miss and retains cleanup accounting. Unreachable Garage refuses startup.
Unfinished reservations become hidden cleanup intents. Unknown objects are never
adopted. Storage identity changes and malformed journals refuse startup.
The prototype/ object prefix remains compatible with the deployed journal.

Read-ticket hashes survive restart without extending their one-minute expiry.
Downloads check expiry before fetching and verify bytes before serving them.
Entry references protect active operations from eviction and deletion.
The bounded background sweeper retries hidden-object deletion with backoff.

The staging directory has separate custody under flock and is wiped on startup.
Published uploads release their spool. Staging is disposable; publication and
recovery authority come from the journal and verified Garage objects.
Close persists metadata and releases custody without deleting published objects.

Tests: TestRestartRecoversVerifiesAndHidesLostObjects,
TestDurableOwnershipAndMalformedState, TestLegacyJournalLoads,
TestRecoveryRefusesStalledGarageWithinBudget, TestJournalFailureFencesBeforeStorage,
TestCrashBeforePublicationLeavesDeletableIntent and TestPublicationReleasesUploadStaging.
