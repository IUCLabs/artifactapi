import os
import hashlib
from urllib.parse import urlparse
import boto3
from botocore.config import Config
from botocore.exceptions import ClientError
from fastapi import HTTPException


class S3Storage:
    def __init__(
        self,
        endpoint: str,
        access_key: str,
        secret_key: str,
        bucket: str,
        secure: bool = False,
    ):
        self.endpoint = endpoint
        self.access_key = access_key
        self.secret_key = secret_key
        self.bucket = bucket
        self.secure = secure

        self.client = boto3.client(
            "s3",
            endpoint_url=f"http{'s' if self.secure else ''}://{self.endpoint}",
            aws_access_key_id=self.access_key,
            aws_secret_access_key=self.secret_key,
            config=Config(
                request_checksum_calculation="when_required",
                response_checksum_validation="when_required"
            )
        )

        # Try to ensure bucket exists, but don't fail if MinIO isn't ready yet
        try:
            self._ensure_bucket_exists()
        except Exception as e:
            print(f"Warning: Could not ensure bucket exists during initialization: {e}")
            print("Bucket creation will be attempted on first use")

    def _ensure_bucket_exists(self):
        try:
            self.client.head_bucket(Bucket=self.bucket)
        except ClientError:
            self.client.create_bucket(Bucket=self.bucket)

    def get_object_key(self, url: str) -> str:
        url_hash = hashlib.sha256(url.encode()).hexdigest()[:16]
        parsed = urlparse(url)
        filename = os.path.basename(parsed.path)
        return f"{parsed.netloc}/{url_hash}/{filename}"

    def get_object_key_from_path(self, remote_name: str, path: str) -> str:
        # Create a key based on the API path for direct access
        path_hash = hashlib.sha256(path.encode()).hexdigest()[:16]
        filename = os.path.basename(path)
        return f"{remote_name}/{path_hash}/{filename}"

    def exists(self, key: str) -> bool:
        try:
            self._ensure_bucket_exists()
            self.client.head_object(Bucket=self.bucket, Key=key)
            return True
        except ClientError:
            return False

    def upload(self, key: str, data: bytes) -> str:
        self._ensure_bucket_exists()
        self.client.put_object(Bucket=self.bucket, Key=key, Body=data)
        return f"s3://{self.bucket}/{key}"

    def get_url(self, key: str) -> str:
        return f"http://{self.endpoint}/{self.bucket}/{key}"

    def get_presigned_url(self, key: str, expiration: int = 3600) -> str:
        try:
            return self.client.generate_presigned_url(
                "get_object",
                Params={"Bucket": self.bucket, "Key": key},
                ExpiresIn=expiration,
            )
        except Exception:
            return self.get_url(key)

    def download_object(self, key: str) -> bytes:
        try:
            self._ensure_bucket_exists()
            response = self.client.get_object(Bucket=self.bucket, Key=key)
            return response["Body"].read()
        except ClientError:
            raise HTTPException(status_code=404, detail="Artifact not found")

    def delete_object(self, key: str) -> bool:
        try:
            self._ensure_bucket_exists()
            self.client.delete_object(Bucket=self.bucket, Key=key)
            return True
        except ClientError:
            return False
