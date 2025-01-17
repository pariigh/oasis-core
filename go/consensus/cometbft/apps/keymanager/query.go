package keymanager

import (
	"context"

	"github.com/oasisprotocol/oasis-core/go/common"
	abciAPI "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/api"
	keymanagerState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/keymanager/state"
	keymanager "github.com/oasisprotocol/oasis-core/go/keymanager/api"
)

// Query is the key manager query interface.
type Query interface {
	Status(context.Context, common.Namespace) (*keymanager.Status, error)
	Statuses(context.Context) ([]*keymanager.Status, error)
	MasterSecret(context.Context, common.Namespace) (*keymanager.SignedEncryptedMasterSecret, error)
	EphemeralSecret(context.Context, common.Namespace) (*keymanager.SignedEncryptedEphemeralSecret, error)
	Genesis(context.Context) (*keymanager.Genesis, error)
}

// QueryFactory is the key manager query factory.
type QueryFactory struct {
	state abciAPI.ApplicationQueryState
}

// QueryAt returns the key manager query interface for a specific height.
func (sf *QueryFactory) QueryAt(ctx context.Context, height int64) (Query, error) {
	state, err := keymanagerState.NewImmutableState(ctx, sf.state, height)
	if err != nil {
		return nil, err
	}
	return &keymanagerQuerier{state}, nil
}

type keymanagerQuerier struct {
	state *keymanagerState.ImmutableState
}

func (kq *keymanagerQuerier) Status(ctx context.Context, id common.Namespace) (*keymanager.Status, error) {
	return kq.state.Status(ctx, id)
}

func (kq *keymanagerQuerier) Statuses(ctx context.Context) ([]*keymanager.Status, error) {
	return kq.state.Statuses(ctx)
}

func (kq *keymanagerQuerier) MasterSecret(ctx context.Context, id common.Namespace) (*keymanager.SignedEncryptedMasterSecret, error) {
	return kq.state.MasterSecret(ctx, id)
}

func (kq *keymanagerQuerier) EphemeralSecret(ctx context.Context, id common.Namespace) (*keymanager.SignedEncryptedEphemeralSecret, error) {
	return kq.state.EphemeralSecret(ctx, id)
}

func (app *keymanagerApplication) QueryFactory() interface{} {
	return &QueryFactory{app.state}
}

// NewQueryFactory returns a new QueryFactory backed by the given state
// instance.
func NewQueryFactory(state abciAPI.ApplicationQueryState) *QueryFactory {
	return &QueryFactory{state}
}
