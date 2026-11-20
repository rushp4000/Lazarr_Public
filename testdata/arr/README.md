# testdata/arr: arr queue/history shapes for the pack janitor

`sonarr_queue_charter_house_s02_partial.json` and `sonarr_history_charter_house_s02.json` are **synthesized** (not a live capture) to the exact Sonarr v4 shapes:
- `QueueResource`, with one record per episode of a multi-episode download, all sharing `downloadId`.
- `TrackedDownloadStatusMessage {title, messages}` and the status messages built by `CompletedDownloadService` (a header "One or more episodes expected…" plus one entry per not-imported file).
- The `UpgradeSpecification` rejection wording.

They model a cached Charter House S02 pack on sonarr_main: 5 episodes were imported and 17 were rejected "Not a Custom Format upgrade". Titles, hashes, ids and paths are fabricated. Distractor items:
- the same shape on another download client;
- the live `.exe` fake (importPending);
- a pack blocked with a non-allowed reason ("Sample");
- a Lazarr Seerr download still in progress.
