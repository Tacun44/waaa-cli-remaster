package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

var chatForwardCmd = &cobra.Command{
	Use:   "forward <from-chat> <message-ref> <to-recipient>",
	Short: "Forward a message to another chat",
	Long: `Forward a message from one chat to another. <message-ref> is the
number shown in 'wa chat open <from-chat>', or a raw WhatsApp message ID.

Text messages and downloaded media references (images, videos, audio,
documents and stickers) can be forwarded when their original message payload
is available in the local history.`,
	Args: cobra.ExactArgs(3),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Position 0 (from-chat) and position 2 (to-recipient) are both
		// chat names; position 1 (message-ref) is a number or message ID,
		// which chatstore can't help complete.
		if len(args) == 0 || len(args) == 2 {
			return completeChatNames(toComplete), cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		fromTarget := args[0]
		ref := args[1]
		toTarget := args[2]

		fromJID, err := resolveJID(fromTarget)
		if err != nil {
			return err
		}
		toJID, err := resolveJID(toTarget)
		if err != nil {
			return err
		}

		ms := msgstore.New(a.Config.DataDir)
		quoted, err := resolveMessageRef(ms, fromJID.String(), ref)
		if err != nil {
			return err
		}

		if err := checkSendGuards(toJID.String()); err != nil {
			return err
		}

		client, _, _, err := connectForSend(ctx)
		if err != nil {
			return err
		}
		defer client.Disconnect()

		_, err = client.ForwardMessage(ctx, toJID, quoted)
		if err != nil {
			return err
		}

		recordSendGuards(toJID.String())

		fmt.Println("Forwarded.")
		return nil
	},
}

func init() {
	chatCmd.AddCommand(chatForwardCmd)
}
