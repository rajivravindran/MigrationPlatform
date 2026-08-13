# Mission: Frontend/backend deployment architecture

## Why
Pass architecture interviews by explaining—and drawing—how web UIs, APIs, and containers communicate. Also become able to run a split frontend/backend layout with Docker and later Kubernetes, using Migration Platform as the concrete example.

## Success looks like
- Draw, from memory, who talks to whom (browser, web container, API, data services) for same-host and two-host layouts
- Explain image vs container vs pod vs Deployment vs Service without mixing them up
- Deploy (or describe step-by-step) web on one place and API stack on another using Docker, then the Kubernetes equivalent
- Answer interview prompts like “How does the frontend reach the backend?” including `NEXT_PUBLIC_API_URL`, published ports, and CORS at a conceptual level

## Constraints
- Prior knowledge is very limited—start from foundations; no rushing
- No hard time limit; prefer durable understanding over speed
- Prefer primary docs (Docker, Kubernetes) over blog summaries

## Out of scope
- Nothing permanently excluded yet: Docker and Kubernetes are both in scope
- Defer deep Temporal internals until the deploy/network mental model is solid
