# Architecture deploy resources

Trusted sources for learning how web UIs, APIs, Docker, and Kubernetes fit together. Prefer these over memory or random blogs.

## Knowledge

- [Docker: What is a container?](https://docs.docker.com/get-started/docker-concepts/the-basics/what-is-a-container/)
  Official beginner explanation: isolated processes, vs VMs, port publishing. Use for: image vs container foundations.
- [Docker: What is an image?](https://docs.docker.com/get-started/docker-concepts/the-basics/what-is-an-image/)
  Official: image as blueprint. Use for: “we deploy an image” interview wording.
- [Docker: Bridge network driver](https://docs.docker.com/engine/network/drivers/bridge/)
  Official: same-host container networking, publish ports, user-defined bridges. Use for: Compose on one machine.
- [Docker: Network drivers overview](https://docs.docker.com/engine/network/drivers/)
  Official: bridge vs host vs none (and others). Use for: when interviewers ask about network modes.
- [Kubernetes: Pods](https://kubernetes.io/docs/concepts/workloads/pods/)
  Official: Pod as smallest deployable unit; usually one container; prefer Deployments. Use for: pod vs container.
- [Kubernetes: Deployments](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/)
  Official: desired replicas, rolling updates. Use for: “two Deployments—web and api”.
- [Kubernetes: Services](https://kubernetes.io/docs/concepts/services-networking/service/)
  Official: stable endpoint in front of Pods. Use for: how traffic reaches changing Pod IPs.
- [MDN: Same-origin policy](https://developer.mozilla.org/en-US/docs/Web/Security/Same-origin_policy)
  Browser security model. Use for: why API and UI on different hosts need CORS.
- [MDN: CORS](https://developer.mozilla.org/en-US/docs/Web/HTTP/Guides/CORS)
  How cross-origin browser calls work. Use for: split frontend/backend hosts.
- [Migration Platform: `apps/web/lib/api.ts`](../../apps/web/lib/api.ts)
  First-party: browser calls `NEXT_PUBLIC_API_URL`. Use for: grounding lessons in this repo.
- [Migration Platform Helm values `NEXT_PUBLIC_API_URL`](../../infra/k8s/helm/values.yaml)
  First-party: intended split URL for API. Use for: K8s-shaped config example.

## Wisdom (Communities)

- [r/kubernetes](https://www.reddit.com/r/kubernetes/)
  Practitioner questions; quality varies—prefer linking to official docs in answers.
- [r/docker](https://www.reddit.com/r/docker/)
  Compose/networking troubleshooting; verify against Docker docs.
- [CNCF Slack / Kubernetes Slack](https://slack.k8s.io/)
  Official community channels for Kubernetes learners (invite via kubernetes site).

## Gaps

- Hands-on multi-machine lab requires a second VM or cloud instance—not documented as a single primary tutorial yet; we will build exercises against Docker Desktop first, then two hosts.
