FROM python:3.12-slim AS base
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
RUN apt-get update && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 app
WORKDIR /app
COPY apps/transform-worker/pyproject.toml /app/pyproject.toml
RUN pip install --no-cache-dir \
    temporalio==1.8.0 \
    orjson==3.10.11 \
    RestrictedPython==7.3 \
    prometheus-client==0.21.0 \
    opentelemetry-sdk==1.27.0 \
    opentelemetry-exporter-otlp-proto-grpc==1.27.0 \
    opentelemetry-api==1.27.0 \
    pydantic==2.9.2

COPY apps/transform-worker /app
USER app
EXPOSE 9464
CMD ["python", "-m", "migration_worker.worker"]
