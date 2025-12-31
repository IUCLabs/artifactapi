import os
from typing import Optional
import psycopg2
from psycopg2.extras import RealDictCursor


class DatabaseManager:
    def __init__(self, db_url: str):
        self.db_url = db_url
        self.available = False
        self._init_database()

    def _init_database(self):
        """Initialize database connection and create schema if needed"""
        try:
            self.connection = psycopg2.connect(self.db_url)
            self.connection.autocommit = True
            self._create_schema()
            self.available = True
            print("Database connection established")
        except Exception as e:
            print(f"Database not available: {e}")
            self.available = False

    def _create_schema(self):
        """Create tables if they don't exist"""
        try:
            with self.connection.cursor() as cursor:
                # Create table to map S3 keys to remote names
                cursor.execute("""
                    CREATE TABLE IF NOT EXISTS artifact_mappings (
                        id SERIAL PRIMARY KEY,
                        s3_key VARCHAR(255) UNIQUE NOT NULL,
                        remote_name VARCHAR(100) NOT NULL,
                        file_path TEXT NOT NULL,
                        size_bytes BIGINT,
                        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
                    )
                """)

                cursor.execute("""
                    CREATE TABLE IF NOT EXISTS local_files (
                        id SERIAL PRIMARY KEY,
                        repository_name VARCHAR(100) NOT NULL,
                        file_path TEXT NOT NULL,
                        s3_key VARCHAR(255) UNIQUE NOT NULL,
                        size_bytes BIGINT NOT NULL,
                        sha256_sum VARCHAR(64) NOT NULL,
                        content_type VARCHAR(100),
                        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
                        uploaded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
                        UNIQUE(repository_name, file_path)
                    )
                """)

                # Create indexes separately
                cursor.execute(
                    "CREATE INDEX IF NOT EXISTS idx_s3_key ON artifact_mappings (s3_key)"
                )
                cursor.execute(
                    "CREATE INDEX IF NOT EXISTS idx_remote_name ON artifact_mappings (remote_name)"
                )
                cursor.execute(
                    "CREATE INDEX IF NOT EXISTS idx_local_repo_path ON local_files (repository_name, file_path)"
                )
                cursor.execute(
                    "CREATE INDEX IF NOT EXISTS idx_local_s3_key ON local_files (s3_key)"
                )
                print("Database schema initialized")
        except Exception as e:
            print(f"Error creating schema: {e}")

    def record_artifact_mapping(
        self, s3_key: str, remote_name: str, file_path: str, size_bytes: int
    ):
        """Record mapping between S3 key and remote"""
        if not self.available:
            return

        try:
            with self.connection.cursor() as cursor:
                cursor.execute(
                    """
                    INSERT INTO artifact_mappings (s3_key, remote_name, file_path, size_bytes)
                    VALUES (%s, %s, %s, %s)
                    ON CONFLICT (s3_key)
                    DO UPDATE SET
                        remote_name = EXCLUDED.remote_name,
                        file_path = EXCLUDED.file_path,
                        size_bytes = EXCLUDED.size_bytes
                """,
                    (s3_key, remote_name, file_path, size_bytes),
                )
        except Exception as e:
            print(f"Error recording artifact mapping: {e}")

    def get_storage_by_remote(self) -> dict[str, int]:
        """Get storage size breakdown by remote from database"""
        if not self.available:
            return {}

        try:
            with self.connection.cursor(cursor_factory=RealDictCursor) as cursor:
                cursor.execute("""
                    SELECT remote_name, SUM(size_bytes) as total_size
                    FROM artifact_mappings
                    GROUP BY remote_name
                """)
                results = cursor.fetchall()
                return {row["remote_name"]: row["total_size"] or 0 for row in results}
        except Exception as e:
            print(f"Error getting storage by remote: {e}")
            return {}

    def get_remote_for_s3_key(self, s3_key: str) -> Optional[str]:
        """Get remote name for given S3 key"""
        if not self.available:
            return None

        try:
            with self.connection.cursor() as cursor:
                cursor.execute(
                    "SELECT remote_name FROM artifact_mappings WHERE s3_key = %s",
                    (s3_key,),
                )
                result = cursor.fetchone()
                return result[0] if result else None
        except Exception as e:
            print(f"Error getting remote for S3 key: {e}")
            return None

    def add_local_file(
        self,
        repository_name: str,
        file_path: str,
        s3_key: str,
        size_bytes: int,
        sha256_sum: str,
        content_type: str = None,
    ):
        """Add a file to local repository"""
        if not self.available:
            return False

        try:
            with self.connection.cursor() as cursor:
                cursor.execute(
                    """
                    INSERT INTO local_files (repository_name, file_path, s3_key, size_bytes, sha256_sum, content_type)
                    VALUES (%s, %s, %s, %s, %s, %s)
                """,
                    (
                        repository_name,
                        file_path,
                        s3_key,
                        size_bytes,
                        sha256_sum,
                        content_type,
                    ),
                )
                self.connection.commit()
                return True
        except Exception as e:
            print(f"Error adding local file: {e}")
            return False

    def get_local_file_metadata(self, repository_name: str, file_path: str):
        """Get metadata for a local file"""
        if not self.available:
            return None

        try:
            with self.connection.cursor() as cursor:
                cursor.execute(
                    """
                    SELECT repository_name, file_path, s3_key, size_bytes, sha256_sum, content_type, created_at, uploaded_at
                    FROM local_files
                    WHERE repository_name = %s AND file_path = %s
                """,
                    (repository_name, file_path),
                )
                result = cursor.fetchone()
                if result:
                    return {
                        "repository_name": result[0],
                        "file_path": result[1],
                        "s3_key": result[2],
                        "size_bytes": result[3],
                        "sha256_sum": result[4],
                        "content_type": result[5],
                        "created_at": result[6],
                        "uploaded_at": result[7],
                    }
                return None
        except Exception as e:
            print(f"Error getting local file metadata: {e}")
            return None

    def list_local_files(self, repository_name: str, prefix: str = ""):
        """List files in local repository with optional path prefix"""
        if not self.available:
            return []

        try:
            with self.connection.cursor() as cursor:
                if prefix:
                    cursor.execute(
                        """
                        SELECT file_path, size_bytes, sha256_sum, content_type, created_at, uploaded_at
                        FROM local_files
                        WHERE repository_name = %s AND file_path LIKE %s
                        ORDER BY file_path
                    """,
                        (repository_name, f"{prefix}%"),
                    )
                else:
                    cursor.execute(
                        """
                        SELECT file_path, size_bytes, sha256_sum, content_type, created_at, uploaded_at
                        FROM local_files
                        WHERE repository_name = %s
                        ORDER BY file_path
                    """,
                        (repository_name,),
                    )

                results = cursor.fetchall()
                return [
                    {
                        "file_path": result[0],
                        "size_bytes": result[1],
                        "sha256_sum": result[2],
                        "content_type": result[3],
                        "created_at": result[4],
                        "uploaded_at": result[5],
                    }
                    for result in results
                ]
        except Exception as e:
            print(f"Error listing local files: {e}")
            return []

    def delete_local_file(self, repository_name: str, file_path: str):
        """Delete a file from local repository"""
        if not self.available:
            return False

        try:
            with self.connection.cursor() as cursor:
                cursor.execute(
                    """
                    DELETE FROM local_files
                    WHERE repository_name = %s AND file_path = %s
                    RETURNING s3_key
                """,
                    (repository_name, file_path),
                )
                result = cursor.fetchone()
                self.connection.commit()
                return result[0] if result else None
        except Exception as e:
            print(f"Error deleting local file: {e}")
            return None

    def file_exists(self, repository_name: str, file_path: str):
        """Check if file exists in local repository"""
        if not self.available:
            return False

        try:
            with self.connection.cursor() as cursor:
                cursor.execute(
                    """
                    SELECT 1 FROM local_files
                    WHERE repository_name = %s AND file_path = %s
                """,
                    (repository_name, file_path),
                )
                return cursor.fetchone() is not None
        except Exception as e:
            print(f"Error checking file existence: {e}")
            return False
