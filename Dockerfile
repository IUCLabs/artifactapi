# Use Alpine Linux as base image
FROM python:3.11-alpine

# Set working directory
WORKDIR /app

# Install system dependencies
RUN apk add --no-cache \
    gcc \
    musl-dev \
    libffi-dev \
    postgresql-dev \
    curl \
    wget \
    tar

# Install uv
ARG PACKAGE_VERSION=0.9.21
RUN wget -O /app/uv-x86_64-unknown-linux-musl.tar.gz https://github.com/astral-sh/uv/releases/download/${PACKAGE_VERSION}/uv-x86_64-unknown-linux-musl.tar.gz && \
    tar xf /app/uv-x86_64-unknown-linux-musl.tar.gz -C /app && \
    mv /app/uv-x86_64-unknown-linux-musl/uv /usr/local/bin/uv && \
    rm -rf /app/uv-x86_64-unknown-linux-musl* && \
    chmod +x /usr/local/bin/uv && \
    uv --version

# Create non-root user first
RUN adduser -D -s /bin/sh appuser && \
    chown -R appuser:appuser /app

# Copy dependency files and change ownership
COPY --chown=appuser:appuser pyproject.toml uv.lock README.md ./

# Switch to appuser and install Python dependencies
USER appuser
ARG VERSION=dev
RUN HATCH_VCS_PRETEND_VERSION=${VERSION} uv sync --frozen

# Copy application source
COPY --chown=appuser:appuser src/ ./src/
COPY --chown=appuser:appuser remotes.yaml ./
COPY --chown=appuser:appuser ca-bundle.pem ./

# Expose port
EXPOSE 8000

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8000/health || exit 1

# Run the application
CMD ["uv", "run", "python", "-m", "src.artifactapi.main"]