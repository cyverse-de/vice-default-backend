# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is **vice-default-backend**, a Go service that acts as the default backend handler for the Kubernetes Ingress routing VICE (Visual Interactive Computing Environment) apps. It determines whether to redirect requests to the loading page service, landing page service, or a 404 page based on URL validity.

## Build and Run Commands

```bash
# Build
go build ./...

# Run locally (requires config file and database)
./vice-default-backend --config /path/to/jobservices.yml --listen 0.0.0.0:60000

# Run with SSL
./vice-default-backend --config /path/to/jobservices.yml --ssl-cert /path/to/cert.crt --ssl-key /path/to/key.key

# Lint (uses shared workflow with golangci-lint)
golangci-lint run
```

## Configuration

The service reads configuration from a YAML file (default: `/etc/iplant/de/jobservices.yml`) with these required keys:
- `vice.db.uri` - PostgreSQL connection string
- `vice.default_backend.base_url` - Base URL for VICE apps
- `vice.default_backend.loading_page_url` - URL for the loading page redirect

## CLI Flags

- `--config` - Path to config file (default: `/etc/iplant/de/jobservices.yml`)
- `--listen` - Listen address (default: `0.0.0.0:60000`)
- `--ssl-cert` / `--ssl-key` - SSL certificate and key paths
- `--static-file-path` - Path to static assets (default: `./static`)
- `--disable-custom-header-match` - Use Host header instead of X-Frontend-Url for subdomain matching (useful for development)
- `--log-level` - One of: trace, debug, info, warn, error, fatal, panic

## Architecture

Single-file Go service (`main.go`) with:
- **App struct** - Holds database connection, URLs, and configuration
- **RouteRequest handler** - Main routing logic that redirects all requests to the loading page with the app URL encoded
- **Health endpoint** - `/healthz` for Kubernetes probes
- **Static file serving** - `/static/` prefix serves files from static directory
- **404 handler** - Serves `static/404.html` for unmatched routes

## Deployment

Uses Skaffold for Kubernetes deployment. Image is built and pushed to `harbor.cyverse.org/de/vice-default-backend`. Kubernetes manifests are in `k8s/vice-default-backend.yml`.
