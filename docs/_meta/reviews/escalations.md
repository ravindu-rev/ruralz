# Escalations register

Review findings that a document's final revision disputed or could not resolve within its own file. Each one also appears in
that document's Open questions table. Source: the review chains of the documentation workflow (2026-09-23 to 2026-09-25).

| Document | Finding | Reason it stayed open | Tracked as | Resolution |
| --- | --- | --- | --- | --- |
| [Multi-Protocol Support](../../architecture/07-multi-protocol.md) | L1-2: the NATS JetStream pre-buffer setting and in-progress acknowledgments need nats.go sources in the research file | The reviser could edit only the document, not `docs/_meta/research/go-libraries-protocols.md` | OQ-multi-protocol-10 | Resolved 2026-09-25: the research patch added verified nats.go and NATS sources (PullMaxMessages pre-buffering, `InProgress()` acknowledgments, AckWait and MaxDeliver defaults) to the research file and the document now cites them; OQ-multi-protocol-10 stays open only for its unrelated design questions |
