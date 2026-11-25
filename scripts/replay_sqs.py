#!/usr/bin/env python3
"""Capture an S3 object set and replay one S3 notification per object."""

import argparse
import json
import os
from pathlib import Path
import subprocess
from urllib.parse import quote_plus


FIREHOSE_PREFIX = "firehose/processed/"


def aws_json(*args, send=False):
    environment = None
    if send:
        # A timed-out Standard SQS send may already have enqueued its messages.
        environment = {**os.environ, "AWS_MAX_ATTEMPTS": "1"}
    result = subprocess.run(
        ["aws", "--no-cli-pager", "--no-cli-auto-prompt", "--output", "json", *args],
        check=True,
        stdout=subprocess.PIPE,
        text=True,
        env=environment,
    )
    return json.loads(result.stdout)


def supported_key(key):
    return key.startswith(FIREHOSE_PREFIX) and key.endswith(".jsonl.gz")


def capture(bucket, prefix):
    if not prefix.startswith(FIREHOSE_PREFIX):
        raise ValueError(f"prefix must start with {FIREHOSE_PREFIX}")
    result = aws_json("s3api", "list-object-versions", "--bucket", bucket, "--prefix", prefix)
    objects = [
        {
            "key": version["Key"],
            "version_id": version["VersionId"],
            "etag": version["ETag"].strip('"'),
            "size_bytes": version["Size"],
        }
        for version in result.get("Versions", [])
        if version["IsLatest"] and supported_key(version["Key"])
    ]
    manifest = {
        "schema_version": 1,
        "bucket": bucket,
        "objects": sorted(objects, key=lambda obj: obj["key"]),
    }
    validate_manifest(manifest)
    return manifest


def validate_manifest(manifest):
    if not isinstance(manifest, dict) or manifest.get("schema_version") != 1:
        raise ValueError("expected manifest schema_version 1")
    bucket = manifest.get("bucket")
    if not isinstance(bucket, str) or not bucket.strip():
        raise ValueError("manifest needs a nonempty S3 bucket name")
    objects = manifest.get("objects")
    if not isinstance(objects, list) or not objects:
        raise ValueError("manifest contains no Firehose .jsonl.gz objects")

    keys = set()
    for index, obj in enumerate(objects, start=1):
        if not isinstance(obj, dict):
            raise ValueError(f"manifest item {index} is not an object")
        key = obj.get("key")
        if not isinstance(key, str) or not supported_key(key) or key in keys:
            raise ValueError(f"invalid or repeated S3 key at manifest item {index}")
        for field in ("version_id", "etag"):
            if not isinstance(obj.get(field), str) or not obj[field].strip():
                raise ValueError(f"manifest item {index} needs {field}")
        if type(obj.get("size_bytes")) is not int or obj["size_bytes"] < 3:
            raise ValueError(f"invalid S3 object size at manifest item {index}")
        keys.add(key)


def notification(bucket, obj):
    s3_object = {"key": quote_plus(obj["key"]), "size": obj["size_bytes"], "eTag": obj["etag"]}
    if obj["version_id"] != "null":
        s3_object["versionId"] = obj["version_id"]
    return {
        "Records": [
            {
                "eventVersion": "2.1",
                "eventSource": "aws:s3",
                "eventName": "ObjectCreated:Put",
                "s3": {"bucket": {"name": bucket}, "object": s3_object},
            }
        ]
    }


def publish(manifest, queue_url, dry_run):
    validate_manifest(manifest)
    if queue_url.rstrip("/").endswith(".fifo"):
        raise ValueError("replay requires a Standard SQS queue")

    bucket = manifest["bucket"]
    objects = manifest["objects"]
    total = len(objects)
    size = sum(obj["size_bytes"] for obj in objects)
    action = "dry run" if dry_run else "publishing"
    print(f"{action}: {total} messages, {size} source bytes from {bucket} to {queue_url}", flush=True)
    if dry_run:
        return

    confirmed = 0
    for start in range(0, total, 10):
        entries = [
            {
                "Id": str(index),
                "MessageBody": json.dumps(notification(bucket, obj), separators=(",", ":")),
            }
            for index, obj in enumerate(objects[start:start + 10], start=start + 1)
        ]
        expected = {entry["Id"] for entry in entries}
        try:
            result = aws_json(
                "sqs", "send-message-batch", "--queue-url", queue_url,
                "--entries", json.dumps(entries), send=True,
            )
        except (OSError, subprocess.CalledProcessError, json.JSONDecodeError, KeyboardInterrupt) as error:
            raise ValueError(
                f"SQS confirmed {confirmed}/{total}; acceptance of the current batch is unknown"
            ) from error
        successful = {entry["Id"] for entry in result.get("Successful", [])}
        confirmed += len(expected & successful)
        failures = result.get("Failed", [])
        if successful != expected or failures:
            failed = ", ".join(f'{entry["Id"]}: {entry["Code"]}' for entry in failures)
            detail = failed or "missing acknowledgments"
            raise ValueError(f"SQS confirmed {confirmed}/{total}; batch incomplete: {detail}")

    print(f"SQS accepted {confirmed}/{total} messages")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    snapshot = commands.add_parser("snapshot", help="save a reusable S3 manifest")
    snapshot.add_argument("--bucket", required=True)
    snapshot.add_argument("--prefix", required=True)
    snapshot.add_argument("--manifest", required=True, type=Path)
    replay = commands.add_parser("publish", help="send the saved S3 notifications to Standard SQS")
    replay.add_argument("--manifest", required=True, type=Path)
    replay.add_argument("--queue-url", required=True)
    replay.add_argument("--dry-run", action="store_true", help="validate and count messages without contacting AWS")
    args = parser.parse_args()

    try:
        if args.command == "snapshot":
            manifest = capture(args.bucket, args.prefix)
            with args.manifest.open("x", encoding="utf-8") as destination:
                json.dump(manifest, destination, ensure_ascii=False, indent=2)
                destination.write("\n")
            print(f"saved {len(manifest['objects'])} objects to {args.manifest}")
        else:
            with args.manifest.open(encoding="utf-8") as source:
                publish(json.load(source), args.queue_url, args.dry_run)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"{parser.prog}: {error}\n")


if __name__ == "__main__":
    main()
