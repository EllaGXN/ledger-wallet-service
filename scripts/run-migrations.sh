#!/bin/sh
# Applies every *.up.sql migration in /migrations, in filename order.
# Down migrations are excluded on purpose — they're for manual rollback,
# not something that should run automatically against a fresh database.
set -e
for f in /migrations/*.up.sql; do
  echo "Applying migration: $f"
  psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -f "$f"
done
