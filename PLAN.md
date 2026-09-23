# zot-annotate plan

`zot-annotate` provides a browser-based review surface for the latest assistant message. The extension keeps the transcript intact and sends structured feedback back through the zot extension protocol.

## Current capabilities

- Register `/annotate` with zot.
- Track the latest `assistant_message` event.
- Serve an embedded annotation UI from a loopback HTTP server.
- Capture selected passages and comments.
- Accept file attachments and store them under the extension data directory.
- Submit annotations as a follow-up prompt to the active session.

## Follow-up work

- [ ] Add automated Go tests for upload limits, configuration, and prompt construction.
- [ ] Vendor the UI stylesheet so the extension works without network access.
- [ ] Add browser acceptance coverage for selection, upload, and submission flows.
- [ ] Add an explicit stop/close action for the annotation server.
- [ ] Add authentication or a one-time token before allowing non-loopback binding.
- [ ] Add support for annotating earlier messages instead of only the latest one.
