# Security policy

## Reporting a vulnerability

Email **amittimalsina21@gmail.com** with a description of the
vulnerability, reproduction steps, and your assessment of the impact.

Please do not open a public GitHub issue for security disclosures.

## Response timeline

- **Acknowledgement**: within 7 days of receipt.
- **Fix**: 14 days for issues affecting API-key handling, RCE, denial-of-service, or anything with CVSS ≥ 7. Other issues addressed on a best-effort basis with a timeline communicated in the acknowledgement.
- **Disclosure**: coordinated. Reporter is credited in the fix's CHANGELOG entry unless they request otherwise.

## Scope

In scope:
- `pi-agent-go` package code, hooks, tool helpers, examples.
- Supply chain: `go.mod` dependencies, GitHub Actions workflows.

Out of scope:
- Vulnerabilities in `pi-llm-go` — report against that repo.
- Vulnerabilities in upstream LLM providers (Anthropic, OpenAI, Azure).
  Forward those to the provider directly.

## Supported versions

Pre-1.0: only the latest minor version receives security fixes.
Post-1.0: the current and previous minor of the latest major.

## Bounty

No bug bounty. Researcher credit in the release notes is the standard
acknowledgement.
