package admin

import (
	"context"
	"fmt"

	"github.com/dolthub/dolt/go/cmd/dolt/cli"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/utils/argparser"
	"github.com/klauspost/compress/zstd"
	"github.com/fatih/color"
)

type ZstdCmd struct {
}

func (cmd ZstdCmd) Name() string {
	return "zstd"
}

func (cmd ZstdCmd) Description() string {
	return "A temporary admin command for taking a dependency on zstd and working out tooling dependencies."
}

func (cmd ZstdCmd) RequiresRepo() bool {
	return false
}

func (cmd ZstdCmd) Docs() *cli.CommandDocumentation {
	return nil
}

func (cmd ZstdCmd) ArgParser() *argparser.ArgParser {
	ap := argparser.NewArgParserWithMaxArgs(cmd.Name(), 0)
	return ap
}

func (cmd ZstdCmd) Hidden() bool {
	return true
}

func (cmd ZstdCmd) Exec(ctx context.Context, commandStr string, args []string, dEnv *env.DoltEnv, cliCtx cli.CliContext) int {
	encoder, _ := zstd.NewWriter(nil)
	compressed := encoder.EncodeAll([]byte("Hello, world!"), nil)
	fmt.Fprintf(color.Error, "Hello, world! compressed is %v\n", compressed)

	return 0
}
