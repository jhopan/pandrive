#!/bin/bash
# Auto backup SQLite database

BACKUP_DIR="./backups"
DB_FILE="./data/pandrive.db"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
BACKUP_FILE="$BACKUP_DIR/pandrive_$TIMESTAMP.db"

mkdir -p "$BACKUP_DIR"

# SQLite backup (handles WAL mode properly)
sqlite3 "$DB_FILE" ".backup '$BACKUP_FILE'"

echo "Backup created: $BACKUP_FILE"

# Keep only last 7 backups
ls -t "$BACKUP_DIR"/pandrive_*.db | tail -n +8 | xargs -r rm

echo "Old backups cleaned (keeping last 7)"
