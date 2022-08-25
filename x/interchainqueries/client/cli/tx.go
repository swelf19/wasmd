package cli

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"strconv"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/client/tx"
	"github.com/spf13/cobra"

	"github.com/neutron-org/neutron/x/interchainqueries/types"
)

// GetTxCmd returns the transaction commands for this module
func GetTxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                        types.ModuleName,
		Short:                      fmt.Sprintf("%s transactions subcommands", types.ModuleName),
		DisableFlagParsing:         true,
		SuggestionsMinimumDistance: 2,
		RunE:                       client.ValidateCmd,
	}

	cmd.AddCommand(SubmitQueryResultCmd())

	return cmd
}

func SubmitQueryResultCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "submit-query-result [query-id] [result-file]",
		Short:   "Submit query result",
		Aliases: []string{"submit", "s"},
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			clientCtx, err := client.GetClientTxContext(cmd)
			if err != nil {
				return err
			}

			sender := clientCtx.GetFromAddress()
			queryID, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse query id: %w", err)
			}

			resultFile := args[1]

			result, err := ioutil.ReadFile(resultFile)
			if err != nil {
				return fmt.Errorf("failed to read query result file: %w", err)
			}

			msg := types.MsgSubmitQueryResult{QueryId: queryID, Sender: string(sender)}
			if err := json.Unmarshal(result, &msg.Result); err != nil {
				return fmt.Errorf("failed to unmarshal query result: %w", err)
			}

			if err = msg.ValidateBasic(); err != nil {
				return err
			}

			return tx.GenerateOrBroadcastTxCLI(clientCtx, cmd.Flags(), &msg)
		},
	}

	flags.AddTxFlagsToCmd(cmd)

	return cmd
}