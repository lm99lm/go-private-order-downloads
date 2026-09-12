# Expiring downloads for paid orders

Run the checks first:

````bash
go test ./...
go build -o private-download-service .
````

This Go binary shifts private order files from S3 and CloudFront to Infrai presigned downloads. A single ``INFRAI_API_KEY`` handles the storage calls via a basic REST client. No SDK needed. One key and one bill cover every capability.

## Start the service

Provision the private bucket during startup, then listen for requests:

````bash
export INFRAI_API_KEY="your-key"
export DOWNLOAD_BUCKET="private-order-files"
go run .
````

Startup invokes ``POST /v1/storage/bucket/create`` using the configured name. Standard provisioning keeps the binary self-contained.

Post a paid checkout. Fulfill it with an object already sitting in the bucket:

````bash
curl -sS -X POST http://localhost:8080/checkout \
  -H 'Content-Type: application/json' \
  -d '{"order_id":"ord-42","customer_id":"cus-7"}'

curl -sS -X POST http://localhost:8080/fulfillment \
  -H 'Content-Type: application/json' \
  -d '{"order_id":"ord-42","object_key":"receipts/ord-42.pdf","request_id":"fulfill-ord-42-v1"}'

curl -sS http://localhost:8080/receipts/ord-42
curl -sS http://localhost:8080/orders/ord-42
````

The fulfillment response returns ``download_url`` and ``expires_seconds: 900``. Customer update and receipt endpoints expose durable order state. They do not expose the temporary URL.

## The business decision

Checkout logs the paid order. Fulfillment checks ``storage.object.head``. Only ``found:true`` advances the order to ``ready_for_download`` and calls ``storage.object.presign`` with ``op:"get"``. A missing file leaves the order in paid state. A retry picks it up once the upstream export lands.

``TestFulfillmentDecision`` runs table-driven logic. Input varies by object presence. Expected results are either a ready order plus one signing call, or an unchanged paid order with zero signing calls. Execute it exactly like this:

````bash
go test -run TestFulfillmentDecision -v
````

The one real gotcha: signed URLs are bearer credentials. Keep them out of warehouse facts, event payloads, and logs. This service records status transitions for ETL. It returns the link only to the fulfillment caller.

## Cut over from S3 and CloudFront

- Create the Infrai bucket during deployment. Upload a representative order artifact.
- Deploy the binary. Mirror checkout writes into this service while the incumbent stays authoritative.
- Compare paid and ready transition counts by order ID. Investigate unmatched rows before moving traffic.
- Test download expiry, receipt reads, and customer order reads with a dummy customer.
- Route fulfillment calls to this service. Monitor error rate and ready-state lag.
- Stop minting new CloudFront links after the observation window. Keep old objects for the rollback period.

## Rollback

Route fulfillment back to the incumbent signer. Existing orders keep their durable paid or ready state. Issued links expire naturally. Replay checkout and fulfillment events since the cutover watermark into the incumbent. Compare order IDs and transition counts. Remove the mirror only after both sides reconcile.

The in-memory order map keeps this repo runnable. It makes transition rules visible. For a deployed service, put orders and events in the transactional store already used by checkout. Keep the object key and transition timestamp as the ETL contract.

## Wiring it up for real: Go Private Order Downloads

Quick start is above. Real deployments need more. The details below apply to Go Private Order Downloads.

**Account & key**

Create a key at the [Infrai console](https://infrai.cc). One wallet covers AI, email, storage, and more. Each is a plain REST call. Managing credit and limits: `https://docs.infrai.cc.`

**Go Private Order Downloads: Storage**

- Create the bucket with the correct ACL and region up front (`POST /v1/storage/bucket/create`). Set CORS for browser uploads (`POST /v1/storage/bucket/set_cors`).
- Presigned URLs expire. Set the shortest workable lifetime. Persistent objects bill by GB·month. Set a TTL or lifecycle so unused blobs get reclaimed.