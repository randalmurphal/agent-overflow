# internal/transferfiles/

Bounded, streaming archives for conversation transfer. No provider formats,
credentials, HTTP handlers, database state, or execution ownership live here.

- Every source root is injected. Callers quiesce writers before snapshotting.
- Only regular files, portable relative paths, and owner execute permission
  cross the boundary. Symlinks, hardlinks, special files, sparse records,
  duplicate/case-alias paths and trailing non-padding data are refused.
- `os.Root` confines path resolution, including concurrent path replacement.
- Extraction creates a new private directory and removes it on failure. A
  verified digest is required before any contents can become prepared state.
- File and directory fsyncs precede success. Installation and ownership commit
  belong to the caller; extraction never overwrites a provider's live files.
- Limits cover files, individual bytes, total bytes, and wire bytes. Everything
  streams; no transcript-sized `ReadAll`, gzip buffers, or persisted read model.
- Uploads use bounded chunks and a durable byte checkpoint. Fsync archive bytes
  before advancing the checkpoint; discard an uncommitted crash tail before the
  next append. A repeated range must match the accepted bytes, and a missing
  accepted prefix is corruption. The caller serializes per operation. Upload
  completion alone does not prepare/activate anything: extraction still verifies
  the whole archive and the coordinator still controls execution ownership.
  `ErrUploadCorrupt` distinguishes malformed checkpoints/lost acknowledged bytes
  from ordinary I/O failures. An UNPREPARED coordinator may reset checkpoint zero
  using the sealed identity stored separately in its journal, then retransmit.
  Never reset after acknowledging preparation or infer a new digest from a
  damaged local file. A reset persists zero before any byte truncation. If extraction fails before
  its digest check, `VerifyUploadContent` distinguishes damaged disk bytes from
  faithfully received invalid archives; only the former may restart upload.
  Healthy uploads never pay a second complete hash pass. One deterministic
  scratch chunk bounds orphan storage across repeated process deaths.
- Installation maps verified members into caller-injected roots. Preparation
  records exact existing digests without writing. Activation accepts only that
  baseline or the exact new file, checks copied bytes, then publishes through a
  confined atomic rename/link. New files use no-clobber publication. Replacing
  an old native session requires caller authorization and the recorded baseline;
  an unknown change is a conflict. Private temporary names are deterministic so
  recovery discards its own incomplete copy instead of accumulating orphans.
  File and ancestor-directory syncs precede success, including an already-done
  retry after a lost reply. App-level fencing and final history activation remain
  the coordinator's responsibility.
