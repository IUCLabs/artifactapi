import hashlib
import logging

from fastapi import HTTPException, Response, UploadFile
from fastapi.responses import JSONResponse

logger = logging.getLogger(__name__)


async def upload(remote_name: str, path: str, file: UploadFile, storage, database, config) -> JSONResponse:
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")
    if remote_config.get("type") != "local":
        raise HTTPException(status_code=400, detail="Upload only supported for local repositories")

    try:
        content = await file.read()
        sha256_sum = hashlib.sha256(content).hexdigest()

        if database.file_exists(remote_name, path):
            raise HTTPException(status_code=409, detail="File already exists")

        s3_key = f"local/{remote_name}/{path}"
        content_type = file.content_type or "application/octet-stream"

        try:
            storage.upload(s3_key, content)
        except Exception as e:
            raise HTTPException(status_code=500, detail=f"Upload failed: {e}")

        success = database.add_local_file(
            repository_name=remote_name,
            file_path=path,
            s3_key=s3_key,
            size_bytes=len(content),
            sha256_sum=sha256_sum,
            content_type=content_type,
        )

        if not success:
            storage.delete_object(s3_key)
            raise HTTPException(status_code=500, detail="Failed to save file metadata")

        return JSONResponse(
            {
                "message": "File uploaded successfully",
                "file_path": path,
                "size_bytes": len(content),
                "sha256_sum": sha256_sum,
                "content_type": content_type,
            }
        )

    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=f"Upload failed: {str(e)}")


def check_exists(remote_name: str, path: str, database, config) -> Response:
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")

    if remote_config.get("type") != "local":
        raise HTTPException(status_code=405, detail="HEAD method only supported for local repositories")

    try:
        metadata = database.get_local_file_metadata(remote_name, path)
        if not metadata:
            raise HTTPException(status_code=404, detail="File not found")

        return Response(
            headers={
                "Content-Length": str(metadata["size_bytes"]),
                "Content-Type": metadata.get("content_type", "application/octet-stream"),
                "X-SHA256": metadata["sha256_sum"],
                "X-Created-At": metadata["created_at"].isoformat() if metadata["created_at"] else "",
                "X-Uploaded-At": metadata["uploaded_at"].isoformat() if metadata["uploaded_at"] else "",
            }
        )
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=f"Check failed: {str(e)}")


def delete(remote_name: str, path: str, storage, database, config) -> JSONResponse:
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")
    if remote_config.get("type") != "local":
        raise HTTPException(status_code=400, detail="Delete only supported for local repositories")

    try:
        s3_key = database.delete_local_file(remote_name, path)
        if not s3_key:
            raise HTTPException(status_code=404, detail="File not found")

        if not storage.delete_object(s3_key):
            logger.warning(f"Failed to delete S3 object {s3_key} after database removal")

        return JSONResponse({"message": "File deleted successfully"})
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=f"Delete failed: {str(e)}")
