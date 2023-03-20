package interchainqueries

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/neutron-org/neutron/x/interchainqueries/keeper"
	"github.com/neutron-org/neutron/x/interchainqueries/types"
)

// InitGenesis initializes the capability module's state from a provided genesis
// state.
func InitGenesis(ctx sdk.Context, k keeper.Keeper, genState types.GenesisState) {
	// Set all registered queries
	for _, elem := range genState.RegisteredQueries {
		k.SetLastRegisteredQueryKey(ctx, elem.Id)
		if err := k.SaveQuery(ctx, *elem); err != nil {
			panic(err)
		}

	}

	k.SetParams(ctx, genState.Params)
}

// ExportGenesis returns the capability module's exported genesis.
func ExportGenesis(ctx sdk.Context, k keeper.Keeper) *types.GenesisState {
	genesis := types.DefaultGenesis()
	genesis.Params = k.GetParams(ctx)

	genesis.RegisteredQueries = k.GetAllRegisteredQueries(ctx)

	return genesis
}