.PHONY: build install dev clean test lint format pre-commit tox docker-build docker-up docker-down docker-logs docker-rebuild docker-clean docker-restart

build:
	docker build --no-cache -t artifactapi:latest .

install: build

docker-build: build

dev: build
	uv sync --dev

clean:
	rm -rf .venv
	rm -rf build/
	rm -rf dist/
	rm -rf *.egg-info/

test:
	uvx --python 3.11 --with tox-uv tox

tox:
	uvx --python 3.11 --with tox-uv tox

pre-commit:
	uvx --python 3.11 pre-commit run --all-files

lint:
	uv run ruff check --fix .

format:
	uv run ruff format .

run:
	uv run python -m src.artifactapi.main

docker-up:
	docker-compose up --build --force-recreate -d

docker-down:
	docker-compose down

docker-logs:
	docker-compose logs -f

docker-rebuild:
	docker-compose build --no-cache

docker-clean:
	docker-compose down -v --remove-orphans
	docker system prune -f

docker-restart: docker-down docker-up

# Bump helpers — reads the latest semver tag and creates the next one.
# If no tag exists yet, starts from v0.0.0.
_LATEST := $(shell git tag --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | head -1)
_BASE   := $(if $(_LATEST),$(_LATEST),v0.0.0)
_MAJ    := $(shell echo $(_BASE) | sed 's/^v//' | cut -d. -f1)
_MIN    := $(shell echo $(_BASE) | sed 's/^v//' | cut -d. -f2)
_PAT    := $(shell echo $(_BASE) | sed 's/^v//' | cut -d. -f3)

patch:
	@NEW=v$(_MAJ).$(_MIN).$(shell expr $(_PAT) + 1); \
	git tag $$NEW && echo "Tagged $$NEW" && $(MAKE) _tag TAG=$$NEW

minor:
	@NEW=v$(_MAJ).$(shell expr $(_MIN) + 1).0; \
	git tag $$NEW && echo "Tagged $$NEW" && $(MAKE) _tag TAG=$$NEW

major:
	@NEW=v$(shell expr $(_MAJ) + 1).0.0; \
	git tag $$NEW && echo "Tagged $$NEW" && $(MAKE) _tag TAG=$$NEW

_tag:
	git push origin $(TAG)
	docker-compose build --no-cache --build-arg VERSION=$(TAG:v%=%)
