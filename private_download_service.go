package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type orderStatus string

const (
	statusPaid  orderStatus = "paid"
	statusReady orderStatus = "ready_for_download"
)

type order struct {
	ID        string      `json:"order_id"`
	Customer  string      `json:"customer_id"`
	ObjectKey string      `json:"object_key,omitempty"`
	Status    orderStatus `json:"status"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type orderEvent struct {
	OrderID    string      `json:"order_id"`
	Status     orderStatus `json:"status"`
	OccurredAt time.Time   `json:"occurred_at"`
}

type downloadStore interface {
	headObject(context.Context, string, string) (bool, error)
	presignDownload(context.Context, string, string, string, time.Duration) (string, error)
}

type orderService struct {
	mu     sync.RWMutex
	bucket string
	store  downloadStore
	orders map[string]order
	events []orderEvent
	now    func() time.Time
}

func newOrderService(bucket string, store downloadStore) *orderService {
	return &orderService{bucket: bucket, store: store, orders: make(map[string]order), now: time.Now}
}

func (s *orderService) checkout(id, customer string) (order, error) {
	if id == "" || customer == "" {
		return order{}, errors.New("order_id and customer_id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.orders[id]; ok {
		return existing, nil
	}
	o := order{ID: id, Customer: customer, Status: statusPaid, UpdatedAt: s.now().UTC()}
	s.orders[id] = o
	s.events = append(s.events, orderEvent{OrderID: id, Status: o.Status, OccurredAt: o.UpdatedAt})
	return o, nil
}

func (s *orderService) fulfill(ctx context.Context, id, key, requestID string) (order, string, error) {
	s.mu.RLock()
	o, ok := s.orders[id]
	s.mu.RUnlock()
	if !ok {
		return order{}, "", errors.New("order not found")
	}
	if o.Status != statusPaid && o.Status != statusReady {
		return order{}, "", errors.New("order is not paid")
	}
	found, err := s.store.headObject(ctx, s.bucket, key)
	if err != nil {
		return order{}, "", err
	}
	if !found {
		return order{}, "", errors.New("fulfillment object is not ready")
	}
	link, err := s.store.presignDownload(ctx, s.bucket, key, requestID, 15*time.Minute)
	if err != nil {
		return order{}, "", err
	}
	s.mu.Lock()
	o.ObjectKey = key
	o.Status = statusReady
	o.UpdatedAt = s.now().UTC()
	s.orders[id] = o
	s.events = append(s.events, orderEvent{OrderID: id, Status: o.Status, OccurredAt: o.UpdatedAt})
	s.mu.Unlock()
	return o, link, nil
}

func (s *orderService) getOrder(id string) (order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[id]
	return o, ok
}

type apiServer struct{ service *orderService }

func (a apiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /checkout", a.checkout)
	mux.HandleFunc("POST /fulfillment", a.fulfillment)
	mux.HandleFunc("GET /receipts/{orderID}", a.receipt)
	mux.HandleFunc("GET /orders/{orderID}", a.customerOrder)
	return mux
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a apiServer) checkout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderID    string `json:"order_id"`
		CustomerID string `json:"customer_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	o, err := a.service.checkout(in.OrderID, in.CustomerID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

func (a apiServer) fulfillment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderID   string `json:"order_id"`
		ObjectKey string `json:"object_key"`
		RequestID string `json:"request_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.OrderID == "" || in.ObjectKey == "" || in.RequestID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order_id, object_key, and request_id are required"})
		return
	}
	o, link, err := a.service.fulfill(r.Context(), in.OrderID, in.ObjectKey, in.RequestID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": o, "download_url": link, "expires_seconds": 900})
}

func (a apiServer) receipt(w http.ResponseWriter, r *http.Request) {
	o, ok := a.service.getOrder(r.PathValue("orderID"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "order not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_id": o.ID, "customer_id": o.Customer, "status": o.Status, "updated_at": o.UpdatedAt})
}

func (a apiServer) customerOrder(w http.ResponseWriter, r *http.Request) {
	o, ok := a.service.getOrder(r.PathValue("orderID"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "order not found"})
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func writeServiceError(w http.ResponseWriter, err error) {
	var apiErr *infraiError
	if errors.As(err, &apiErr) && apiErr.HTTPStatus >= 400 && apiErr.HTTPStatus < 500 {
		writeJSON(w, apiErr.HTTPStatus, map[string]string{"error": apiErr.Message})
		return
	}
	if strings.Contains(err.Error(), "not found") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if strings.Contains(err.Error(), "not ready") || strings.Contains(err.Error(), "not paid") {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "storage request failed"})
}

func main() {
	key := os.Getenv("INFRAI_API_KEY")
	if key == "" {
		log.Fatal("INFRAI_API_KEY is required")
	}
	bucket := envOr("DOWNLOAD_BUCKET", "private-order-files")
	client := newInfraiClient(key)
	if err := client.createBucket(context.Background(), bucket); err != nil {
		log.Fatalf("prepare download bucket: %v", err)
	}
	server := &http.Server{Addr: envOr("LISTEN_ADDR", ":8080"), Handler: apiServer{newOrderService(bucket, client)}.routes(), ReadHeaderTimeout: 5 * time.Second}
	fmt.Printf("private download service listening on %s\n", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
