# Taste
- Wants everything written in Chinese: replies, documentation, code comments, and commit messages (「语言都用中文」). Confidence: 0.9
- Wants code comments kept to the necessary minimum — only non-obvious "why" and constraint explanations; drop comments that merely restate the code. Confidence: 0.7
- Wants durable conventions (like the Chinese-language rule) written into the repo's AGENTS.md so they're explicit, reusable by the team and other agents. Confidence: 0.75
- Prompts are terse — often just a topic label (「迭代1」) — and expects the agent to ask clarifying questions and align scope before producing large deliverables. Confidence: 0.55
- Prefers incremental delivery: split an oversized stage into milestones and freeze only the first slice, rather than freezing a large design in one go. Confidence: 0.6
- Prefers to freeze a design document (scope, invariants, API/CLI, migration, error codes, tests, acceptance criteria, open questions) before implementing. Confidence: 0.55
- Requires verification claims to be honest: use ports + fake adapters + shared contract tests so work can progress locally, and explicitly mark anything not exercised on the real target (Linux host, systemd, real services) as 「未验证」. Confidence: 0.65
- Prefers validating Linux-only semantics (systemd, file modes, Unix Socket ACLs, users/groups) in Docker/Linux containers instead of waiting for a real Linux host, and wants container evidence recorded as its own evidence category, never conflated with real-host evidence. Confidence: 0.6
- Prefers capturing a validated technique/convention in documentation right away and deferring the supporting harness/tooling until it is actually needed, rather than building verification infrastructure preemptively. Confidence: 0.5
