#!/bin/bash
set -e
make build && docker compose restart boss1
