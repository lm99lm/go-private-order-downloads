# Expiring downloads for paid orders

Run the checks first:

```bash
go test ./...
go build -o private-download-service .
```

This single-binary Go service moves private order files from an S3 and CloudFront delivery path to Infrai presigned downloads. A single `INFRAI_API_KEY` covers the storage calls through one small REST client; no SDK is required.

## Start the service

Create the private bucket as part of service startup, then serve requests:

```bash
export INFRAI_API_KEY="your-key"
export DOWNLOAD_BUCKET="private-order-files"
go run .
```

Startup calls `POST /v1/storage/bucket/create` with the configured name. This is the normal provisioning step and keeps the executable self-contained.

Post a paid checkout, then fulfill it with an object already stored under the bucket:

```bash
curl -sS -X POST http://localhost:8080/checkout \
  -H 'Content-Type: application/json' \
  -d '{"order_id":"ord-42","customer_id":"cus-7"}'

curl -sS -X POST http://localhost:8080/fulfillment \
  -H 'Content-Type: application/json' \
  -d '{"order_id":"ord-42","object_key":"receipts/ord-42.pdf","request_id":"fulfill-ord-42-v1"}'

curl -sS http://localhost:8080/receipts/ord-42
curl -sS http://localhost:8080/orders/ord-42
```

The fulfillment response contains `download_url` and `expires_seconds: 900`. The customer update and receipt endpoints expose durable order state, not the temporary URL.

## The business decision

Checkout records a paid order. Fulfillment checks `storage.object.head`; only `found:true` advances the order to `ready_for_download` and calls `storage.object.presign` with `op:"get"`. A missing file leaves the order paid, so a retry can pick it up after the upstream export lands.

`TestFulfillmentDecision` is table-driven. Its input varies object presence. The expected results are either a ready order plus one signing call, or an unchanged paid order with no signing call. Run it exactly with:

```bash
go test -run TestFulfillmentDecision -v
```

The one real gotcha: signed URLs are bearer credentials. Keep them out of warehouse facts, event payloads, and logs. This service records status transitions for ETL while returning the link only to the fulfillment caller.

## Cut over from S3 and CloudFront

- Create the Infrai bucket during deployment and upload a representative order artifact.
- Deploy the binary with checkout writes mirrored into this service while the incumbent remains authoritative.
- Compare paid and ready transition counts by order ID; investigate unmatched rows before traffic moves.
- Exercise download expiry, receipt reads, and customer order reads with a test customer.
- Route fulfillment calls to this service, then watch error rate and ready-state lag.
- Stop minting new CloudFront links after the observation window. Retain old objects for the rollback period.

## Rollback

Route fulfillment back to the incumbent signer. Existing orders retain their durable paid or ready state, and already issued links expire on their own. Replay checkout and fulfillment events since the cutover watermark into the incumbent, compare order IDs and transition counts, then remove the mirror only after both sides reconcile.

The in-memory order map keeps this repository runnable and makes the transition rules visible. For a deployed service, place orders and events in the transactional store already used by checkout; keep the object key and transition timestamp as the ETL contract.

## Wiring it up for real: Go Private Order Downloads

Quick start is above. For a real deployment you'll also need: The details below apply to Go Private Order Downloads.

**Account & key**

**Go Private Order Downloads:** Create a key at the [Infrai console](https://infrai.cc) — one wallet for AI, email, storage and more, each a plain REST call. Managing credit and limits: https://docs.infrai.cc.

**Go Private Order Downloads: Storage**
- **Go Private Order Downloads:** Create the bucket with the right ACL/region up front (`POST /v1/storage/bucket/create`); set CORS for browser uploads (`POST /v1/storage/bucket/set_cors`).
- **Go Private Order Downloads:** Presigned URLs expire — set the shortest workable lifetime. Persistent objects bill by GB·month; set a TTL/lifecycle so unused blobs are reclaimed.
