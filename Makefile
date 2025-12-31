.PHONY: build install dev clean test lint format docker-build docker-up docker-down docker-logs docker-rebuild docker-clean docker-restart

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
	uv run pytest

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
