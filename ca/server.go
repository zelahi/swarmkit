package ca

import (
	"fmt"
	"sync"

	"github.com/docker/swarm-v2/api"
	"github.com/docker/swarm-v2/identity"
	"github.com/docker/swarm-v2/log"
	"github.com/docker/swarm-v2/manager/state"
	"github.com/docker/swarm-v2/manager/state/store"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// Server is the CA API gRPC server.
type Server struct {
	mu               sync.Mutex
	acceptancePolicy api.AcceptancePolicy
	wg               sync.WaitGroup
	ctx              context.Context
	cancel           func()
	store            *store.MemoryStore
	securityConfig   *ManagerSecurityConfig
}

// DefaultAcceptancePolicy returns the default acceptance policy.
func DefaultAcceptancePolicy() api.AcceptancePolicy {
	return api.AcceptancePolicy{
		Autoaccept: map[string]bool{AgentRole: true},
	}
}

// NewServer creates a CA API server.
func NewServer(store *store.MemoryStore, securityConfig *ManagerSecurityConfig, acceptancePolicy api.AcceptancePolicy) *Server {
	return &Server{
		acceptancePolicy: acceptancePolicy,
		store:            store,
		securityConfig:   securityConfig,
	}
}

// CertificateStatus returns the current issuance status of an issuance request identified by Token
func (s *Server) CertificateStatus(ctx context.Context, request *api.CertificateStatusRequest) (*api.CertificateStatusResponse, error) {
	if request.Token == "" {
		return nil, grpc.Errorf(codes.InvalidArgument, codes.InvalidArgument.String())
	}

	var rCertificate *api.RegisteredCertificate

	// We create a watcher before checking the cert so we can be sure we don't miss any events
	event := state.EventUpdateRegisteredCertificate{
		RegisteredCertificate: &api.RegisteredCertificate{ID: request.Token},
		Checks:                []state.RegisteredCertificateCheckFunc{state.RegisteredCertificateCheckID},
	}

	updates, cancel := state.Watch(s.store.WatchQueue(), event)
	defer cancel()

	// Retrieve the current value of the certificate with this token
	s.store.View(func(tx store.ReadTx) {
		rCertificate = store.GetRegisteredCertificate(tx, request.Token)
	})
	// This token doesn't exist
	if rCertificate == nil {
		return nil, grpc.Errorf(codes.NotFound, codes.NotFound.String())
	}

	log.G(ctx).Debugf("(*Server).CertificateStatus: token %s is in state: %s", request.Token, rCertificate.Status)

	// If this certificate has a final state, return it immediately
	if rCertificate.Status.State != api.IssuanceStatePending {
		return &api.CertificateStatusResponse{
			Status:                &rCertificate.Status,
			RegisteredCertificate: rCertificate,
		}, nil
	}

	log.G(ctx).Debugf("(*Server).CertificateStatus: watching for updates on token=%s.", request.Token)

	// Certificate is Pending or in an Unknown state, let's wait for changes.
L:
	for {
		select {
		case event := <-updates:
			switch v := event.(type) {
			case state.EventUpdateRegisteredCertificate:
				// We got an update on the certificate record. If the status is no
				// longer pending, return.
				if v.RegisteredCertificate.Status.State != api.IssuanceStatePending {
					rCertificate = v.RegisteredCertificate
					break L
				}
			}
		case <-ctx.Done():
			break L
		}
	}

	return &api.CertificateStatusResponse{
		Status:                &rCertificate.Status,
		RegisteredCertificate: rCertificate,
	}, nil
}

// IssueCertificate receives requests from a remote client indicating a node type and a CSR,
// returning a certificate chain signed by the local CA, if available.
func (s *Server) IssueCertificate(ctx context.Context, request *api.IssueCertificateRequest) (*api.IssueCertificateResponse, error) {
	if request.CSR == nil || request.Role == "" {
		return nil, grpc.Errorf(codes.InvalidArgument, codes.InvalidArgument.String())
	}

	var token string

	// Max number of collisions of ID or CN to tolerate before giving up
	maxRetries := 3

	// Generate a random token for this new node
	for i := 0; ; i++ {
		token = identity.NewID()
		nodeID := identity.NewID()

		var certificate *api.RegisteredCertificate
		err := s.store.Update(func(tx store.Tx) error {
			conflictingCNs, err := store.FindRegisteredCertificates(tx, store.ByCN(nodeID))
			if err != nil {
				return err
			}
			if len(conflictingCNs) != 0 {
				return store.ErrExist
			}

			certificate = &api.RegisteredCertificate{
				ID:   token,
				CSR:  request.CSR,
				CN:   nodeID,
				Role: request.Role,
				Status: api.IssuanceStatus{
					State: api.IssuanceStatePending,
				},
			}
			return store.CreateRegisteredCertificate(tx, certificate)
		})
		if err == nil {
			log.G(ctx).Debugf("(*Server).IssueCertificate: added issue certificate entry for Role=%s with Token=%s", request.Role, token)
			break
		}
		if err != store.ErrExist {
			return nil, err
		}
		if i == maxRetries {
			return nil, err
		}
	}

	return &api.IssueCertificateResponse{
		Token: token,
	}, nil
}

// GetRootCACertificate returns the certificate of the Root CA.
func (s *Server) GetRootCACertificate(ctx context.Context, request *api.GetRootCACertificateRequest) (*api.GetRootCACertificateResponse, error) {

	log.G(ctx).Debugf("(*Server).GetRootCACertificate called ")

	return &api.GetRootCACertificateResponse{
		Certificate: s.securityConfig.RootCA.Cert,
	}, nil
}

// Run runs the CA signer main loop.
// The CA signer can be stopped with cancelling ctx or calling Stop().
func (s *Server) Run(ctx context.Context) error {
	if !s.securityConfig.RootCA.CanSign() {
		return fmt.Errorf("no valid signer for Root CA found")
	}

	s.mu.Lock()
	if s.isRunning() {
		s.mu.Unlock()
		return fmt.Errorf("CA signer is stopped")
	}
	s.wg.Add(1)
	defer s.wg.Done()
	logger := log.G(ctx).WithField("module", "ca")
	ctx = log.WithLogger(ctx, logger)
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.mu.Unlock()

	var (
		rCerts []*api.RegisteredCertificate
		err    error
	)
	updates, cancel, err := store.ViewAndWatch(s.store, func(readTx store.ReadTx) error {
		rCerts, err = store.FindRegisteredCertificates(readTx, store.All)
		return err
	})
	if err != nil {
		log.G(ctx).WithError(err).Errorf("snapshot store update failed")
		return err
	}
	defer cancel()

	if err := s.reconcileCertificates(ctx, rCerts); err != nil {
		// We don't return here because that means the Run loop would
		// never run. Log an error instead.
		log.G(ctx).WithError(err).Errorf("error attempting to reconcile certificates")
	}

	// Watch for changes.
	for {
		select {
		case event := <-updates:
			switch v := event.(type) {
			case state.EventCreateRegisteredCertificate:
				s.evaluateAndSignCert(ctx, v.RegisteredCertificate)
			case state.EventUpdateRegisteredCertificate:
				s.evaluateAndSignCert(ctx, v.RegisteredCertificate)
			}

		case <-s.ctx.Done():
			return nil
		}
	}
}

// Stop stops dispatcher and closes all grpc streams.
func (s *Server) Stop() error {
	s.mu.Lock()
	if !s.isRunning() {
		return fmt.Errorf("CA signer is already stopped")
	}
	s.cancel()
	s.mu.Unlock()
	// wait for all handlers to finish their raft deals, because manager will
	// set raftNode to nil
	s.wg.Wait()
	return nil
}

func (s *Server) addTask() error {
	s.mu.Lock()
	if !s.isRunning() {
		s.mu.Unlock()
		return grpc.Errorf(codes.Aborted, "CA signer is stopped")
	}
	s.wg.Add(1)
	s.mu.Unlock()
	return nil
}

func (s *Server) doneTask() {
	s.wg.Done()
}

func (s *Server) isRunning() bool {
	if s.ctx == nil {
		return false
	}
	select {
	case <-s.ctx.Done():
		return false
	default:
	}
	return true
}

func (s *Server) setCertState(rCertificate *api.RegisteredCertificate, state api.IssuanceState) error {
	return s.store.Update(func(tx store.Tx) error {
		latestCertificate := store.GetRegisteredCertificate(tx, rCertificate.ID)
		if latestCertificate == nil {
			return store.ErrNotExist
		}

		// Remote users are expecting a full certificate chain, not just a signed certificate
		latestCertificate.Status = api.IssuanceStatus{
			State: state,
		}

		return store.UpdateRegisteredCertificate(tx, latestCertificate)
	})
}

func (s *Server) evaluateAndSignCert(ctx context.Context, rCertificate *api.RegisteredCertificate) {
	// FIXME(aaronl): Right now, this automatically signs any pending certificate. We need to
	// add more flexible logic on acceptance modes.

	// If the desired state and actual state are in sync, there's nothing
	// to do.
	if rCertificate.Spec.DesiredState == rCertificate.Status.State {
		return
	}

	// If the desired state of a certificate was set to rejected or
	// blocked, we should set the actual state according to those
	// wishes right away, and that is all that should be done.
	if rCertificate.Spec.DesiredState == api.IssuanceStateRejected {
		err := s.setCertState(rCertificate, api.IssuanceStateRejected)
		if err != nil {
			log.G(ctx).WithError(err).Errorf("(*Server).evaluateAndSignCert: failed to change certificate state")
		}
		return
	}
	if rCertificate.Spec.DesiredState == api.IssuanceStateBlocked {
		err := s.setCertState(rCertificate, api.IssuanceStateBlocked)
		if err != nil {
			log.G(ctx).WithError(err).Errorf("(*Server).evaluateAndSignCert: failed to change certificate state")
		}
		return
	}

	if rCertificate.Status.State != api.IssuanceStatePending {
		return
	}

	if s.acceptancePolicy.Autoaccept[rCertificate.Role] {
		s.signCert(ctx, rCertificate)
		return
	}

	if rCertificate.Spec.DesiredState == api.IssuanceStateIssued {
		// Cert was approved by admin
		s.signCert(ctx, rCertificate)
	}
}

func (s *Server) signCert(ctx context.Context, rCertificate *api.RegisteredCertificate) {
	cert, err := s.securityConfig.RootCA.ParseValidateAndSignCSR(rCertificate.CSR, rCertificate.CN, rCertificate.Role)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("(*Server).signCert: failed to parse CSR")
	}

	err = s.store.Update(func(tx store.Tx) error {
		latestCertificate := store.GetRegisteredCertificate(tx, rCertificate.ID)
		if latestCertificate == nil {
			log.G(ctx).Errorf("(*Server).signCert: registered certificate not found in store")
		}

		// Remote users are expecting a full certificate chain, not just a signed certificate
		latestCertificate.Certificate = append(cert, s.securityConfig.RootCA.Cert...)
		latestCertificate.Status = api.IssuanceStatus{
			State: api.IssuanceStateIssued,
		}

		return store.UpdateRegisteredCertificate(tx, latestCertificate)
	})
	if err != nil {
		log.G(ctx).WithError(err).Errorf("(*Server).signCert: transaction failed")
	}
	log.G(ctx).Debugf("(*Server).signCert: issued certificate for Node=%s and Role=%s", rCertificate.CN, rCertificate.Role)
}

func (s *Server) reconcileCertificates(ctx context.Context, rCerts []*api.RegisteredCertificate) error {
	for _, rCert := range rCerts {
		s.evaluateAndSignCert(ctx, rCert)
	}

	return nil
}
