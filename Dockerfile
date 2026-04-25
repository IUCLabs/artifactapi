FROM registry.example.com/example/almalinux9-base:latest

ARG VERSION=0.0.0.dev0

COPY . /build

RUN HATCH_VCS_PRETEND_VERSION=${VERSION} \
    SETUPTOOLS_SCM_PRETEND_VERSION=${VERSION} \
    uv build --wheel --directory /build && \
    useradd -m -r -s /bin/sh appuser

USER appuser
RUN uv tool install --from /build/dist/*.whl artifactapi

USER root
RUN rm -rf /build

EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 CMD curl -f http://localhost:8000/health || exit 1
USER appuser
ENV PATH="/home/appuser/.local/bin:$PATH"
WORKDIR /app
CMD ["artifactapi"]
