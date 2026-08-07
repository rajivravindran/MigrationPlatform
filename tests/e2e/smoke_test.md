# E2E Smoke Test

1. Start infra using `docker compose -f infra/docker-compose.yml up -d`.
2. Start API, orchestrator, transform worker, and web app.
3. Create a rule template.
4. Upload CSV.
5. Trigger job.
6. Pause, resume, cancel, and retry one row.
7. Verify row status transitions in job console.
