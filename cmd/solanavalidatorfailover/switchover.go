package solanavalidatorfailover

import (
	"github.com/rs/zerolog/log"
	"github.com/sol-strategies/solana-validator-failover/internal/config"
	"github.com/sol-strategies/solana-validator-failover/internal/validator"
	"github.com/spf13/cobra"
)

var (
	swAutoConfirm bool
	swDryRun      bool
	swToPeer      string
	switchoverCmd = &cobra.Command{
		Use:          "switchover",
		Short:        "orchestrate a planned switchover between validator nodes",
		SilenceUsage: true,
		Run: func(cmd *cobra.Command, args []string) {
			cfg, err := config.NewFromFile(configPath)
			if err != nil {
				log.Fatal().Err(err).Msg("failed to load config")
			}

			orchestrator, err := validator.NewSwitchoverOrchestrator(&cfg.Validator, configPath)
			if err != nil {
				log.Fatal().Err(err).Msg("failed to create switchover orchestrator")
			}

			err = orchestrator.Switchover(validator.SwitchoverParams{
				AutoConfirm: swAutoConfirm,
				DryRun:      swDryRun,
				ToPeer:      swToPeer,
			})
			if err != nil {
				log.Fatal().Err(err).Msg("switchover failed")
			}
		},
	}
)

func init() {
	switchoverCmd.Flags().BoolVarP(&swAutoConfirm, "yes", "y", false, "automatically answer yes to all prompts (skip interactive menu)")
	switchoverCmd.Flags().BoolVar(&swDryRun, "dry-run", false, "simulate the switchover without actually switching identities")
	switchoverCmd.Flags().StringVar(&swToPeer, "to-peer", "", "specify the target peer by name (skips selection prompt)")
	rootCmd.AddCommand(switchoverCmd)
}
