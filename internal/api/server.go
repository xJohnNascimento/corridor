package api

import (
	"net/http"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"

	"go.temporal.io/sdk/client"

	"github.com/xJohnNascimento/corridor/api/corridor/v1/corridorv1connect"
	"github.com/xJohnNascimento/corridor/internal/config"
	"github.com/xJohnNascimento/corridor/internal/failpoint"
	"github.com/xJohnNascimento/corridor/internal/provider"
	"github.com/xJohnNascimento/corridor/internal/store"
)

type Deps struct {
	Store          *store.Store
	Config         *config.Config
	Policy         config.Policy
	Provider       provider.Provider
	Temporal       client.Client
	Points         *failpoint.Points
	AuditorKeyHash []byte
	WebhookSecrets [][]byte
	AuditStore     *store.Store
	DatasetID      string
}

type Server struct {
	store          *store.Store
	cfg            *config.Config
	policy         config.Policy
	provider       provider.Provider
	temporal       client.Client
	points         *failpoint.Points
	auditor        []byte
	webhookSecrets [][]byte
	audit          *store.Store
	datasetID      string
}

func New(d Deps) *Server {
	return &Server{
		store: d.Store, cfg: d.Config, policy: d.Policy, provider: d.Provider,
		temporal: d.Temporal, points: d.Points, auditor: d.AuditorKeyHash, webhookSecrets: d.WebhookSecrets,
		audit: d.AuditStore, datasetID: d.DatasetID,
	}
}

// CustomerHandler serves the customer port: quotes, payments and commands.
func (s *Server) CustomerHandler() http.Handler {
	mux := newMux(corridorv1connect.QuoteServiceName, corridorv1connect.PaymentServiceName, corridorv1connect.CommandServiceName)
	auth := connect.WithInterceptors(customerKeys(s.store))
	mux.Handle(corridorv1connect.NewQuoteServiceHandler(quoteService{s}, auth))
	mux.Handle(corridorv1connect.NewPaymentServiceHandler(paymentService{s}, auth))
	mux.Handle(corridorv1connect.NewCommandServiceHandler(commandService{s}, auth))
	return mux
}

// InternalHandler serves the internal port: operators, the auditor and provider webhooks. It is
// never exposed publicly.
func (s *Server) InternalHandler() http.Handler {
	mux := newMux(corridorv1connect.OpsServiceName)
	mux.Handle(corridorv1connect.NewOpsServiceHandler(opsService{Server: s}, connect.WithInterceptors(internalKeys(s.store, s.auditor))))
	mux.HandleFunc("POST /webhooks/ditto", s.webhookInbox)
	return mux
}

func newMux(services ...string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(services...)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
