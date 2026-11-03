"""Exercise CloudNest authentication and real data operations using the standard library."""

import argparse
import datetime
import hashlib
import hmac
import http.client
import json
import socket
import subprocess
import uuid
import xml.etree.ElementTree as ET
from urllib.parse import quote, urlencode

from check_infra_ports import COMPOSE, docker_json


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def service_port(service, target):
    return next(int(port["published"]) for port in service["ports"] if port["target"] == target)


def verify_mysql(service):
    port = service_port(service, 3306)
    with socket.create_connection(("127.0.0.1", port), timeout=10) as connection:
        handshake = connection.recv(4096)
    require(b"8.4.13" in handshake, "Host MySQL port did not return the expected server handshake")
    table = "__infra_probe_" + uuid.uuid4().hex
    sql = (
        f"CREATE TABLE cloudnest.{table} (value VARCHAR(64) NOT NULL);"
        f"INSERT INTO cloudnest.{table} VALUES ('cloudnest-ready');"
        f"SELECT value FROM cloudnest.{table};"
        f"DROP TABLE cloudnest.{table};"
        "SELECT VERSION(), @@character_set_server;"
    )
    result = subprocess.run(
        ["docker", "run", "--rm", "--network", "cloudnest-dev",
         "--env", "MYSQL_PWD=" + service["environment"]["MYSQL_ROOT_PASSWORD"],
         service["image"], "mysql", "--host=mysql", "--user=root",
         "--batch", "--skip-column-names", "--execute", sql],
        check=True, capture_output=True, text=True,
    )
    require("cloudnest-ready" in result.stdout and "utf8mb4" in result.stdout, "MySQL read/write check failed")
    print("PASS mysql: mapped host port, root authentication, network DNS, SQL read/write, utf8mb4")


def redis_reply(reader):
    prefix = reader.read(1)
    line = reader.readline().rstrip(b"\r\n")
    if prefix == b"-":
        raise RuntimeError(line.decode())
    if prefix in (b"+", b":"):
        return line
    if prefix == b"$":
        length = int(line)
        if length < 0:
            return None
        data = reader.read(length)
        require(reader.read(2) == b"\r\n", "Invalid Redis bulk response")
        return data
    raise RuntimeError("Unexpected Redis response")


def redis_command(connection, reader, *arguments):
    encoded = [str(argument).encode() for argument in arguments]
    payload = f"*{len(encoded)}\r\n".encode()
    for argument in encoded:
        payload += f"${len(argument)}\r\n".encode() + argument + b"\r\n"
    connection.sendall(payload)
    return redis_reply(reader)


def verify_redis(service):
    port = service_port(service, 6379)
    environment = service["environment"]
    with socket.create_connection(("127.0.0.1", port), timeout=10) as connection:
        reader = connection.makefile("rb")
        try:
            redis_command(connection, reader, "PING")
        except RuntimeError as error:
            require("NOAUTH" in str(error), "Redis rejected anonymous access for an unexpected reason")
        else:
            raise RuntimeError("Redis unexpectedly permits anonymous access")
        require(redis_command(connection, reader, "AUTH", environment["REDIS_USERNAME"], environment["REDIS_PASSWORD"]) == b"OK", "Redis ACL authentication failed")
        key = "cloudnest:infra-probe:" + uuid.uuid4().hex
        require(redis_command(connection, reader, "SET", key, "ready", "EX", 60) == b"OK", "Redis SET failed")
        require(redis_command(connection, reader, "GET", key) == b"ready", "Redis GET failed")
        require(redis_command(connection, reader, "DEL", key) == b"1", "Redis DEL failed")
    print("PASS redis: mapped host port, anonymous rejection, root ACL authentication, SET/GET/DEL")


class S3Probe:
    def __init__(self, service):
        self.port = service_port(service, 9000)
        self.access_key = service["environment"]["MINIO_ROOT_USER"]
        self.secret_key = service["environment"]["MINIO_ROOT_PASSWORD"]

    def request(self, method, path, body=b"", query=None, extra_headers=None):
        now = datetime.datetime.now(datetime.UTC)
        timestamp = now.strftime("%Y%m%dT%H%M%SZ")
        day = now.strftime("%Y%m%d")
        host = f"127.0.0.1:{self.port}"
        canonical_query = "&".join(
            quote(str(key), safe="-_.~") + "=" + quote(str(value), safe="-_.~")
            for key, value in sorted((query or {}).items())
        )
        payload_hash = hashlib.sha256(body).hexdigest()
        signed_headers = "host;x-amz-content-sha256;x-amz-date"
        canonical_headers = (
            f"host:{host}\nx-amz-content-sha256:{payload_hash}\nx-amz-date:{timestamp}\n"
        )
        canonical_request = "\n".join(
            [method, path, canonical_query, canonical_headers, signed_headers, payload_hash]
        )
        scope = f"{day}/us-east-1/s3/aws4_request"
        string_to_sign = "\n".join(
            ["AWS4-HMAC-SHA256", timestamp, scope,
             hashlib.sha256(canonical_request.encode()).hexdigest()]
        )
        signing_key = ("AWS4" + self.secret_key).encode()
        for item in (day, "us-east-1", "s3", "aws4_request"):
            signing_key = hmac.new(signing_key, item.encode(), hashlib.sha256).digest()
        signature = hmac.new(signing_key, string_to_sign.encode(), hashlib.sha256).hexdigest()
        headers = {
            "Host": host, "x-amz-content-sha256": payload_hash, "x-amz-date": timestamp,
            "Authorization": f"AWS4-HMAC-SHA256 Credential={self.access_key}/{scope}, "
                             f"SignedHeaders={signed_headers}, Signature={signature}",
        }
        headers.update(extra_headers or {})
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=30)
        try:
            connection.request(method, path + ("?" + canonical_query if canonical_query else ""), body, headers)
            response = connection.getresponse()
            data = response.read()
            response_headers = dict(response.getheaders())
            return response.status, response_headers, data
        finally:
            connection.close()


def xml_value(body, name):
    return ET.fromstring(body).findtext("{*}" + name)


def verify_minio(service):
    probe = S3Probe(service)
    bucket = "/cloudnest"
    status, _, _ = probe.request("HEAD", bucket)
    if status == 404:
        status, _, body = probe.request("PUT", bucket)
        require(status == 200, f"Create private cloudnest bucket failed: {status} {body[:200]!r}")
    else:
        require(status == 200, f"Bucket access failed: {status}")
    object_path = bucket + "/infra-probe-" + uuid.uuid4().hex
    upload_id = None
    aborted_id = None
    try:
        first_part = b"CloudNest" * (5 * 1024 * 1024 // 9 + 1)
        second_part = b"multipart-tail"
        expected = first_part + second_part
        status, _, body = probe.request("POST", object_path, query={"uploads": ""})
        require(status == 200, f"Multipart initialization failed: {status} {body[:200]!r}")
        upload_id = xml_value(body, "UploadId")
        require(bool(upload_id), "Multipart response has no upload ID")
        tags = []
        for number, part in enumerate((first_part, second_part), 1):
            status, headers, body = probe.request(
                "PUT", object_path, part, {"partNumber": number, "uploadId": upload_id}
            )
            require(status == 200, f"Upload part failed: {status} {body[:200]!r}")
            tags.append(next(value for key, value in headers.items() if key.lower() == "etag"))
        status, _, body = probe.request("GET", object_path, query={"uploadId": upload_id})
        require(status == 200 and len(ET.fromstring(body).findall("{*}Part")) == 2, "ListParts failed")
        completion = "<CompleteMultipartUpload>" + "".join(
            f"<Part><PartNumber>{number}</PartNumber><ETag>{tag}</ETag></Part>"
            for number, tag in enumerate(tags, 1)
        ) + "</CompleteMultipartUpload>"
        status, _, body = probe.request("POST", object_path, completion.encode(), {"uploadId": upload_id})
        require(status == 200 and ET.fromstring(body).tag.split("}")[-1] != "Error", "Multipart completion failed")
        upload_id = None
        status, _, downloaded = probe.request("GET", object_path)
        require(status == 200 and hashlib.sha256(downloaded).digest() == hashlib.sha256(expected).digest(), "S3 content hash differs")
        status, _, ranged = probe.request("GET", object_path, extra_headers={"Range": "bytes=0-15"})
        require(status == 206 and ranged == expected[:16], "S3 Range download failed")
        connection = http.client.HTTPConnection("127.0.0.1", probe.port, timeout=10)
        try:
            connection.request("GET", object_path)
            response = connection.getresponse()
            response.read()
            require(response.status == 403, "Private S3 object permits anonymous reads")
        finally:
            connection.close()
        status, _, body = probe.request("POST", object_path + "-abort", query={"uploads": ""})
        require(status == 200, "Abort test initialization failed")
        aborted_id = xml_value(body, "UploadId")
        status, _, _ = probe.request("DELETE", object_path + "-abort", query={"uploadId": aborted_id})
        require(status == 204, "AbortMultipartUpload failed")
        aborted_id = None
    finally:
        if upload_id:
            probe.request("DELETE", object_path, query={"uploadId": upload_id})
        if aborted_id:
            probe.request("DELETE", object_path + "-abort", query={"uploadId": aborted_id})
        probe.request("DELETE", object_path)
    print("PASS minio: private cloudnest bucket, SigV4, Multipart/ListParts/complete/abort, SHA256, Range, anonymous rejection")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--service", choices=["all", "mysql", "redis", "minio"], default="all")
    selected = parser.parse_args().service
    config = docker_json([*COMPOSE[1:], "config", "--format", "json"])
    for name, verifier in (("mysql", verify_mysql), ("redis", verify_redis), ("minio", verify_minio)):
        if selected in ("all", name):
            verifier(config["services"][name])


if __name__ == "__main__":
    main()
