# How Melinda was built

I'm Sarp Aydemir. I built Melinda by directing AI coding agents under rules I set, not by typing the code myself.
This page says how that worked, with numbers from the repository history.

## The setup

Three things guided the agents, all kept outside this repository:

- a project rules file (`CLAUDE.md`): scope, workflow, engineering rules, what must never be published;
- architecture decision records for the choices that are hard to reverse: package boundaries, persistence,
  configuration, the adapter model;
- a behaviour specification: what the agent must do, written down before the code that does it.

A project-manager agent coordinated the work across specialised agents: an architect, engineers, a reviewer, a QA
engineer, a security auditor, a devops engineer and a technical writer. The architect split milestones into issues,
an engineer took one issue to one pull request, and the others tested, audited and reviewed it.

## The numbers

Melinda was developed in a private development repository through 79 pull requests and 92 issues, merged between
2026-10-31 and 2026-11-01, one pull request per unit of work and each one through `make check`. This repository
carries that history, with the pull request numbers taken out of the commit messages.

## The gates

Every pull request passed `make check` locally (gofmt, `go vet`, staticcheck, tests with the race detector), and
`make test-integration` when SQL changed. Nearly every pull request got a reviewer's verdict, and most merged after at
most one round of fixes; only blocking findings held a merge. Later work added end-to-end tests, a soak test and a vulnerability scan.
Merges went through a script that re-ran the gates in a clean checkout.

I set the direction and the rules and made the key calls: the name, the licence, the privacy defaults, what ships.
Day-to-day planning and trade-offs were delegated to the project-manager agent within those rules. Every change passed
the review gates.

## Why it worked

- Small units. A pull request that fits in a reviewer's head gets a real review.
- An explicit contract. The agents built against a written specification and decisions, so they did not guess.
- Tests that assert observed behaviour. Golden tests compare output with recorded results instead of what someone
  assumed the output should be.
- Local gates that anyone, human or agent, runs the same way.
- Adversarial review and long-running tests. Reviewers were told to find problems, and reviews and tests found real
  ones before they shipped. Four examples:
  - **SSRF.** The security review of the page fetch adapter found address forms that reached private ranges: NAT64,
    6to4, Teredo and IPv4-in-IPv6 spellings of loopback and link-local addresses. The re-check after the fix confirmed
    it, including 24 loopback spellings against a listener.
  - **Memory bomb.** Reading an HTML page with many unclosed formatting tags made the parser clone them into every
    later block: a 200 KiB page allocated 797 MiB. The fix changed how those tags are read; the same page now takes
    16 MiB.
  - **Lost messages.** The soak test dropped the websocket on a schedule and found that a message posted during a
    disconnect could go unanswered, because live posts on the new connection moved the replay marker past it. The fix
    records the marker before connecting.
  - **Self-reply loop.** The pre-production security review found that a bot ID belonging to another account made the
    agent answer its own direct messages. Startup now verifies the account and refuses to run on a mismatch.

## What this does not mean

Agent-written code can still be wrong. The gates and reviews reduce that risk; they don't remove it. This is
pre-release software. Read the code before you trust it with your team's data.
