package telegram

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"watgbridge/database"
	"watgbridge/state"
	"watgbridge/utils"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type waTgBridgeCommand struct {
	command     handlers.Command
	description string
}

var commands = []waTgBridgeCommand{}

func AddTelegramHandlers() {
	var (
		cfg        = state.State.Config
		dispatcher = state.State.TelegramDispatcher
	)

	dispatcher.AddHandlerToGroup(handlers.NewMessage(
		func(msg *gotgbot.Message) bool {
			return msg.Chat.Id == cfg.Telegram.TargetChatID
		}, BridgeTelegramToWhatsAppHandler,
	), DispatcherForwardHandlerGroup)

	commands = append(commands,
		waTgBridgeCommand{
			handlers.NewCommand("start", StartCommandHandler),
			"",
		},
		waTgBridgeCommand{
			handlers.NewCommand("getwagroups", GetWhatsAppGroupsHandler),
			"Get all the WhatsApp groups along with their JIDs",
		},
		waTgBridgeCommand{
			handlers.NewCommand("findcontact", FindContactHandler),
			"Fuzzy find contact JIDs from names in WhatsApp",
		},
		waTgBridgeCommand{
			handlers.NewCommand("revoke", RevokeCommandHandler),
			"Revoke a message from WhatsApp",
		},
		waTgBridgeCommand{
			handlers.NewCommand("synccontacts", SyncContactsHandler),
			"Try to sync the contacts list from WhatsApp",
		},
		waTgBridgeCommand{
			handlers.NewCommand("clearpairhistory", ClearMessageIdPairsHistoryHandler),
			"Delete all the past stored message id pairs",
		},
		waTgBridgeCommand{
			handlers.NewCommand("restartwa", RestartWhatsAppConnectionHandler),
			"Restart the WhatsApp client",
		},
		waTgBridgeCommand{
			handlers.NewCommand("joininvitelink", JoinInviteLinkHandler),
			"Join a WhatsApp chat using invite link",
		},
		waTgBridgeCommand{
			handlers.NewCommand("settargetgroupchat", SetTargetGroupChatHandler),
			"Set the target WhatsApp group chat for current thread",
		},
		waTgBridgeCommand{
			handlers.NewCommand("settargetprivatechat", SetTargetPrivateChatHandler),
			"Set the target WhatsApp private chat for current thread",
		},
		waTgBridgeCommand{
			handlers.NewCommand("unlinkthread", UnlinkThreadHandler),
			"Unlink the current thread from its WhatsApp chat",
		},
		waTgBridgeCommand{
			handlers.NewCommand("getprofilepicture", GetProfilePictureHandler),
			"Get the profile picture of user or group using its ID",
		},
		waTgBridgeCommand{
			handlers.NewCommand("updateandrestart", UpdateAndRestartHandler),
			"Try to fetch updates from GitHub and build and restart the bot",
		},
		waTgBridgeCommand{
			handlers.NewCommand("synctopicnames", SyncTopicNamesHandler),
			"Update the names of the topics created",
		},
		waTgBridgeCommand{
			handlers.NewCommand("send", SendToWhatsAppHandler),
			"Send a message to WhatsApp",
		},
		waTgBridgeCommand{
			handlers.NewCommand("help", HelpCommandHandler),
			"Get all the available commands",
		},
		waTgBridgeCommand{
			handlers.NewCommand("block", BlockCommandHandler),
			"Block a user in WhatsApp",
		},
		waTgBridgeCommand{
			handlers.NewCommand("unblock", UnblockCommandHandler),
			"Unblock a user in WhatsApp",
		},
		waTgBridgeCommand{
			handlers.NewCommand("menu", MenuCommandHandler),
			"Open the interactive control panel menu",
		},
		waTgBridgeCommand{
			handlers.NewCommand("ai", AIChatHandler),
			"Ask Gemini AI to respond or draft a message",
		},
	)

	for _, command := range commands {
		dispatcher.AddHandler(command.command)
		if command.description != "" {
			state.State.TelegramCommands = append(state.State.TelegramCommands,
				gotgbot.BotCommand{
					Command:     command.command.Command,
					Description: command.description,
				},
			)
		}
	}

	dispatcher.AddHandlerToGroup(handlers.NewCallback(
		func(cq *gotgbot.CallbackQuery) bool {
			return strings.HasPrefix(cq.Data, "revoke")
		}, RevokeCallbackHandler), DispatcherCallbackHandlerGroup)

	dispatcher.AddHandlerToGroup(handlers.NewCallback(
		func(cq *gotgbot.CallbackQuery) bool {
			return strings.HasPrefix(cq.Data, "menu")
		}, MenuCallbackHandler), DispatcherCallbackHandlerGroup)

	dispatcher.AddHandlerToGroup(handlers.NewCallback(
		func(cq *gotgbot.CallbackQuery) bool {
			return strings.HasPrefix(cq.Data, "ai_send_") || strings.HasPrefix(cq.Data, "ai_discard_")
		}, AIPendingCallbackHandler), DispatcherCallbackHandlerGroup)
}

func BridgeTelegramToWhatsAppHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	for _, command := range commands {
		if command.command.CheckUpdate(b, c) {
			return nil
		}
	}

	var (
		waClient     = state.State.WhatsAppClient
		msgToForward = c.EffectiveMessage
		msgToReplyTo = c.EffectiveMessage.ReplyToMessage
	)

	var stanzaID, participantID, waChatID string
	var quotedWaChatID string
	var err error

	if msgToReplyTo != nil && msgToReplyTo.ForumTopicCreated == nil {
		stanzaID, participantID, waChatID, err = database.MsgIdGetWaFromTg(c.EffectiveChat.Id, msgToReplyTo.MessageId, msgToForward.MessageThreadId)
		if err != nil {
			return utils.TgReplyWithErrorByContext(b, c, "Failed to retreive a pair from database", err)
		} else if stanzaID == "" {
			return utils.TgReplyWithErrorByContext(b, c, "Cannot send to WhatsApp", fmt.Errorf("corresponding stanza Id to replied to message not found"))
		}

		if waChatID == waClient.Store.ID.String() {
			waChatID = participantID
		}
		quotedWaChatID = waChatID
	} else {
		waChatID, err = database.ChatThreadGetWaFromTg(c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId)
		if err != nil {
			return utils.TgReplyWithErrorByContext(b, c, "Failed to find the chat pairing between this topic and a WhatsApp chat", err)
		} else if waChatID == "" {
			if c.EffectiveMessage.MessageThreadId != 0 {
				_, err = utils.TgReplyTextByContext(b, c, "No mapping found between current topic and a WhatsApp chat", nil, false)
				return err
			}
			return nil
		}
	}

	if msgToForward.ExternalReply != nil && msgToForward.ExternalReply.Chat != nil && msgToForward.ExternalReply.MessageId != 0 {
		stanzaID, participantID, quotedWaChatID, err = database.MsgIdGetWaFromTgMessage(
			msgToForward.ExternalReply.Chat.Id,
			msgToForward.ExternalReply.MessageId,
		)
		if err != nil {
			return utils.TgReplyWithErrorByContext(b, c, "Failed to retreive a pair from database", err)
		} else if stanzaID == "" {
			return utils.TgReplyWithErrorByContext(b, c, "Cannot send to WhatsApp", fmt.Errorf("corresponding stanza Id to replied to message not found"))
		}
	} else if quotedWaChatID == "" {
		quotedWaChatID = waChatID
	}

	if stanzaID != "" && !utils.WaReplyContextAllowed(waChatID, quotedWaChatID, participantID) {
		return utils.TgReplyWithErrorByContext(b, c, "Cannot send to WhatsApp",
			fmt.Errorf("cross-chat reply is only allowed for WhatsApp status replies or private replies to a group message sender"))
	}

	// Status Update
	if strings.HasSuffix(waChatID, "@broadcast") {
		waChatID = participantID

		waChatJID, _ := utils.WaParseJID(participantID)
		contactName := utils.WaGetContactName(waChatJID)

		contactThreadID, err := utils.TgGetOrMakeThreadFromWa(waChatJID, c.EffectiveChat.Id, contactName)
		if err != nil {
			return utils.TgReplyWithErrorByContext(b, c, "Failed to get or create a thread for the contact", err)
		}

		forwardedMsg, err := b.ForwardMessage(c.EffectiveChat.Id, c.EffectiveChat.Id, c.EffectiveMessage.MessageId, &gotgbot.ForwardMessageOpts{
			MessageThreadId: contactThreadID,
		})
		if err != nil {
			return utils.TgReplyWithErrorByContext(b, c, "Failed to forward the message to the contact's thread", err)
		}

		msgCopy := *msgToForward
		msgCopy.MessageId = forwardedMsg.MessageId
		msgCopy.MessageThreadId = forwardedMsg.MessageThreadId

		finalWaChatJID, _ := utils.WaParseJID(waChatID)
		return utils.TgSendToWhatsApp(b, c, &msgCopy, msgToReplyTo, finalWaChatJID, participantID, stanzaID, quotedWaChatID, true)

	} else if participantID != "" {
		participant, _ := utils.WaParseJID(participantID)
		participantID = participant.ToNonAD().String()
	}

	waChatJID, _ := utils.WaParseJID(waChatID)

	return utils.TgSendToWhatsApp(b, c, msgToForward, msgToReplyTo, waChatJID, participantID, stanzaID, quotedWaChatID, stanzaID != "")
}

func StartCommandHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	var (
		startTime     = state.State.StartTime
		localLocation = state.State.LocalLocation
		timeFormat    = state.State.Config.TimeFormat
		upTime        = time.Now().UTC().Sub(startTime).Round(time.Second)
	)

	startMessage := "Hi! The bot is up and running\n\n"
	startMessage += fmt.Sprintf("• <b>Up Since</b>: %s [ %s ]\n",
		startTime.In(localLocation).Format(timeFormat),
		upTime.String(),
	)
	startMessage += fmt.Sprintf("• <b>Version</b>: <code>%s</code>\n", state.WATGBRIDGE_VERSION)
	if len(state.State.Modules) > 0 {
		startMessage += "• <b>Loaded Modules</b>:\n"
		for _, module := range state.State.Modules {
			startMessage += fmt.Sprintf("  - <i>%s</i>\n", html.EscapeString(module))
		}
	} else {
		startMessage += "• No Modules Loaded\n"
	}

	_, err := utils.TgReplyTextByContext(b, c, startMessage, nil, false)
	return err
}

func GetWhatsAppGroupsHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	waClient := state.State.WhatsAppClient

	waGroups, err := waClient.GetJoinedGroups(context.Background())
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to retrieve the groups", err)
	}

	outputString := ""
	for groupNum, group := range waGroups {
		outputString += fmt.Sprintf("%v. %s [ <code>%s</code> ]\n",
			groupNum+1, html.EscapeString(group.Name),
			html.EscapeString(group.JID.String()))

		if len(outputString) >= 1800 {
			utils.TgReplyTextByContext(b, c, outputString, nil, false)
			time.Sleep(500 * time.Millisecond)
			outputString = ""
		}
	}

	if len(outputString) > 0 {
		_, err = utils.TgReplyTextByContext(b, c, outputString, nil, false)
		return err
	}
	return nil
}

func FindContactHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	usageString := "Usage : <code>" + html.EscapeString("/findcontact <search_string>") + "</code>\n"
	usageString += "Example : <code>/findcontact propheci</code>"

	args := c.Args()
	if len(args) <= 1 {
		_, err := utils.TgReplyTextByContext(b, c, usageString, nil, false)
		return err
	}
	query := args[1]

	results, resultsCount, err := utils.WaFuzzyFindContacts(query)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Encountered error while finding contacts", err)
	} else if resultsCount == 0 {
		_, err = utils.TgReplyTextByContext(b, c, "No matching results found :(", nil, false)
		return err
	}

	outputString := fmt.Sprintf("Here are the %v matching contacts:\n\n", resultsCount)
	for jid, name := range results {
		outputString += fmt.Sprintf("- <i>%s</i> [ <code>%s</code> ]\n",
			html.EscapeString(name), html.EscapeString(jid))

		if len(outputString) >= 1800 {
			utils.TgReplyTextByContext(b, c, outputString, nil, false)
			time.Sleep(500 * time.Millisecond)
			outputString = ""
		}
	}

	if len(outputString) > 0 {
		_, err = utils.TgReplyTextByContext(b, c, outputString, nil, false)
		return err
	}
	return nil
}

func UpdateAndRestartHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	cfg := state.State.Config

	if cfg.UseGithHubBinaries {
		if cfg.Architecture == "" {
			return utils.TgReplyWithErrorByContext(b, c,
				"Please set an architecture field in config file\nCan be 'amd64' or 'aarch64'",
				nil)
		}

		RELEASE_URL_FORMAT := "https://github.com/akshettrj/watgbridge/releases/latest/download/watgbridge_linux_%s"

		url := fmt.Sprintf(RELEASE_URL_FORMAT, cfg.Architecture)
		err := utils.DownloadFileToLocalByURL("watgbridge_temp", url)
		if err != nil {
			return utils.TgReplyWithErrorByContext(b, c, "Failed to download the release", err)
		}

		err = os.Rename("watgbridge_temp", "watgbridge")
		if err != nil {
			return utils.TgReplyWithErrorByContext(b, c, "Failed to rename the downloaded file", err)
		}

		err = os.Chmod("watgbridge", 0755)
		if err != nil {
			return utils.TgReplyWithErrorByContext(b, c, "Failed to make the file executable", err)
		}

		utils.TgReplyTextByContext(b, c, "Successfully downloaded and prepared the release, now restarting...", nil, false)

	} else {
		gitPullCmd := exec.Command(cfg.GitExecutable, "pull", "--rebase")
		err := gitPullCmd.Run()
		if err != nil {
			return utils.TgReplyWithErrorByContext(b, c, "Failed to execute 'git pull --rebase' command", err)
		}

		utils.TgReplyTextByContext(b, c, "Successfully pulled from GitHub", nil, false)

		goBuildCmd := exec.Command(cfg.GoExecutable, "build")
		err = goBuildCmd.Run()
		if err != nil {
			return utils.TgReplyWithErrorByContext(b, c, "Failed to execute 'go build' command", err)
		}

		utils.TgReplyTextByContext(b, c, "Successfully built the binary, now restarting...", nil, false)

	}

	os.Setenv("WATG_IS_RESTARTED", "1")
	os.Setenv("WATG_CHAT_ID", fmt.Sprint(c.EffectiveChat.Id))
	os.Setenv("WATG_MESSAGE_ID", fmt.Sprint(c.EffectiveMessage.MessageId))

	err := syscall.Exec(path.Join(".", "watgbridge"), []string{}, os.Environ())
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to run exec syscall to restart the bot", err)
	}

	return nil
}

func SyncContactsHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	utils.TgReplyTextByContext(b, c, "Starting syncing contacts... may take some time", nil, false)

	waClient := state.State.WhatsAppClient

	err := waClient.FetchAppState(context.Background(), appstate.WAPatchCriticalUnblockLow, false, false)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to sync contacts", err)
	}

	contacts, err := waClient.Store.Contacts.GetAllContacts(context.Background())
	if err == nil {
		database.ContactNameBulkAddOrUpdate(contacts)
	}

	_, err = utils.TgReplyTextByContext(b, c, "Successfully synced the contact list", nil, false)
	return err
}

func ClearMessageIdPairsHistoryHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	err := database.MsgIdDropAllPairs()
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to delete stored pairs", err)
	}

	_, err = utils.TgReplyTextByContext(b, c, "Successfully deleted all the stored pairs", nil, false)
	return err
}

func RestartWhatsAppConnectionHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	waClient := state.State.WhatsAppClient

	waClient.Disconnect()
	err := waClient.Connect()
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to reconnect to WA servers", err)
	}

	_, err = utils.TgReplyTextByContext(b, c, "Successfully restarted the WhatsApp connection", nil, false)
	return err
}

func JoinInviteLinkHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	usageString := "Usage: <code>" + html.EscapeString("/joininvitelink <invite_link>") + "</code>"

	args := c.Args()
	if len(args) <= 1 {
		_, err := utils.TgReplyTextByContext(b, c, usageString, nil, false)
		return err
	}
	inviteLink := args[1]

	waClient := state.State.WhatsAppClient

	groupID, err := waClient.JoinGroupWithLink(context.Background(), inviteLink)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to join", err)
	}

	_, err = utils.TgReplyTextByContext(b, c,
		fmt.Sprintf("Joined a new group with ID: <code>%s</code>", groupID.String()), nil, false)
	return err
}

func SetTargetGroupChatHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	usageString := "Usage: (Send in a topic) <code>" + html.EscapeString("/settargetgroupchat <group_id>") + "</code>"

	args := c.Args()
	if len(args) <= 1 {
		_, err := utils.TgReplyTextByContext(b, c, usageString, nil, false)
		return err
	}

	if !c.EffectiveMessage.IsTopicMessage || c.EffectiveMessage.MessageThreadId == 0 {
		_, err := utils.TgReplyTextByContext(b, c, "The command should be sent in a topic", nil, false)
		return err
	}

	var (
		cfg      = state.State.Config
		groupID  = args[1]
		waClient = state.State.WhatsAppClient
	)

	groupJID, _ := utils.WaParseJID(groupID)
	groupInfo, err := waClient.GetGroupInfo(context.Background(), groupJID)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to get group info", err)
	}
	groupJID = groupInfo.JID

	_, threadFound, err := database.ChatThreadGetTgFromWa(groupJID.String(), cfg.Telegram.TargetChatID)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to check database for existing mapping", err)
	} else if threadFound {
		_, err = utils.TgReplyTextByContext(b, c, "A topic already exists in database for the given WhatsApp chat. Aborting...", nil, false)
		return err
	}

	err = database.ChatThreadAddNewPair(groupJID.String(), cfg.Telegram.TargetChatID, c.EffectiveMessage.MessageThreadId)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to add the mapping in database. Unsuccessful", err)
	}

	_, err = utils.TgReplyTextByContext(b, c, "Successfully mapped", nil, false)
	return err
}

func UnlinkThreadHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	if !c.EffectiveMessage.IsTopicMessage || c.EffectiveMessage.MessageThreadId == 0 {
		_, err := utils.TgReplyTextByContext(b, c, "The command should be sent in a topic", nil, false)
		return err
	}

	var (
		tgChatId   = c.EffectiveChat.Id
		tgThreadId = c.EffectiveMessage.MessageThreadId
	)

	waChatId, err := database.ChatThreadGetWaFromTg(tgChatId, tgThreadId)
	if err != nil {
		err = utils.TgReplyWithErrorByContext(b, c, "Failed to get existing chat ID pairing", err)
		return err
	} else if waChatId == "" {
		_, err := utils.TgReplyTextByContext(b, c, "No existing chat pairing found!!", nil, false)
		return err
	}

	err = database.ChatThreadDropPairByTg(tgChatId, tgThreadId)
	if err != nil {
		err = utils.TgReplyWithErrorByContext(b, c, "Failed to delete the thread chat pairing", err)
		return err
	}

	_, err = utils.TgReplyTextByContext(b, c, "Successfully unlinked", nil, false)
	return err
}

func handleBlockUnblockUser(b *gotgbot.Bot, c *ext.Context, action events.BlocklistChangeAction) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}
	if !c.EffectiveMessage.IsTopicMessage || c.EffectiveMessage.MessageThreadId == 0 {
		_, err := utils.TgReplyTextByContext(b, c, "The command should be sent in a topic", nil, false)
		return err
	}

	var (
		tgChatId   = c.EffectiveChat.Id
		tgThreadId = c.EffectiveMessage.MessageThreadId
	)

	waChatId, err := database.ChatThreadGetWaFromTg(tgChatId, tgThreadId)
	if err != nil {
		err = utils.TgReplyWithErrorByContext(b, c, "Failed to get existing chat ID pairing", err)
		return err
	} else if waChatId == "" {
		_, err := utils.TgReplyTextByContext(b, c, "No existing chat pairing found!!", nil, false)
		return err
	}
	jid, _ := utils.WaParseJID(waChatId)
	_, err = state.State.WhatsAppClient.UpdateBlocklist(context.Background(), jid, action)
	if err != nil {
		err = utils.TgReplyWithErrorByContext(b, c, "Failed to change the blocklist status", err)
		return err
	}
	actionText := "blocked"
	if action == events.BlocklistChangeActionUnblock {
		actionText = "unblocked"
	}

	_, err = utils.TgReplyTextByContext(b, c, fmt.Sprintf("Successfully %s the user", actionText), nil, false)
	return err
}

func BlockCommandHandler(b *gotgbot.Bot, c *ext.Context) error {
	return handleBlockUnblockUser(b, c, events.BlocklistChangeActionBlock)
}

func UnblockCommandHandler(b *gotgbot.Bot, c *ext.Context) error {
	return handleBlockUnblockUser(b, c, events.BlocklistChangeActionUnblock)
}

func SetTargetPrivateChatHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	usageString := "Usage (Send in a topic): <code>" + html.EscapeString("/settargetprivatechat <user_id>") + "</code>"

	args := c.Args()
	if len(args) <= 1 {
		_, err := utils.TgReplyTextByContext(b, c, usageString, nil, false)
		return err
	}

	if !c.EffectiveMessage.IsTopicMessage || c.EffectiveMessage.MessageThreadId == 0 {
		_, err := utils.TgReplyTextByContext(b, c, "The command should be sent in a topic", nil, false)
		return err
	}

	var (
		cfg     = state.State.Config
		groupID = args[1]
	)

	userJID, _ := utils.WaParseJID(groupID)

	_, threadFound, err := database.ChatThreadGetTgFromWa(userJID.String(), cfg.Telegram.TargetChatID)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to check database for existing mapping", err)
	} else if threadFound {
		_, err = utils.TgReplyTextByContext(b, c, "A topic already exists in database for the given WhatsApp chat. Aborting...", nil, false)
		return err
	}

	err = database.ChatThreadAddNewPair(userJID.String(), cfg.Telegram.TargetChatID, c.EffectiveMessage.MessageThreadId)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to add the mapping in database. Unsuccessful", err)
	}

	_, err = utils.TgReplyTextByContext(b, c, "Successfully mapped", nil, false)
	return err
}

func GetProfilePictureHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	usageString := "Usage: <code>" + html.EscapeString("/getprofilepicture <user/group_id>") + "</code>"
	usageString += "\n\nYou need to add <code>@g.us</code> at the end for groups"

	args := c.Args()
	if len(args) <= 1 {
		_, err := utils.TgReplyTextByContext(b, c, usageString, nil, false)
		return err
	}

	var (
		waClient = state.State.WhatsAppClient
		userID   = args[1]
	)

	userJID, _ := utils.WaParseJID(userID)

	ppInfo, err := waClient.GetProfilePictureInfo(context.Background(), userJID, &whatsmeow.GetProfilePictureParams{})
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to fetch profile picture info from WhatsApp", err)
	}

	res, err := http.DefaultClient.Get(ppInfo.URL)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to make HTTP GET request to profile picture URL", err)
	}
	defer res.Body.Close()

	imgBytes, err := io.ReadAll(res.Body)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to read HTTP response body", err)
	}

	opts := &gotgbot.SendPhotoOpts{
		ReplyParameters: &gotgbot.ReplyParameters{
			MessageId: c.EffectiveMessage.MessageId,
		},
	}
	if c.EffectiveMessage.IsTopicMessage {
		opts.MessageThreadId = c.EffectiveMessage.MessageThreadId
	}
	_, err = b.SendPhoto(c.EffectiveChat.Id, &gotgbot.FileReader{Data: bytes.NewReader(imgBytes)}, opts)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "Failed to send photo", err)
	}

	return nil
}

func SyncTopicNamesHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	chatThreadPairs, err := database.ChatThreadGetAllPairs(c.EffectiveChat.Id)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "failed to retreive chat thread pairs from database", err)
	}

	for _, pair := range chatThreadPairs {
		var (
			waChatId   = pair.ID
			tgThreadId = pair.TgThreadId
		)

		if waChatId == "status@broadcast" || waChatId == "calls" || waChatId == "mentions" {
			continue
		}
		waChatJid, _ := utils.WaParseJID(waChatId)

		var newName string
		if waChatJid.Server == waTypes.GroupServer {
			newName = utils.WaGetGroupName(waChatJid)
		} else {
			newName = utils.WaGetContactName(waChatJid)
		}

		b.EditForumTopic(c.EffectiveChat.Id, tgThreadId, &gotgbot.EditForumTopicOpts{
			Name:              newName,
			IconCustomEmojiId: nil,
		})
		time.Sleep(5 * time.Second)
	}

	_, err = c.EffectiveMessage.Reply(b, "Successfully synced topic names", nil)
	return err
}

func HelpCommandHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	helpString := "Here are the available commands:\n\n"

	for _, command := range state.State.TelegramCommands {
		helpString += fmt.Sprintf("- <code>/%s</code> : %s\n",
			command.Command, html.EscapeString(command.Description))
	}

	_, err := utils.TgReplyTextByContext(b, c, helpString, nil, false)
	return err
}

func SendToWhatsAppHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	usageString := "Usage : Reply to a message, <code>" + html.EscapeString("/send <target_id>") + "</code>\n"
	usageString += "Example : <code>/send 911234567890</code>"

	args := c.Args()
	if len(args) <= 1 || c.EffectiveMessage.ReplyToMessage == nil || c.EffectiveMessage.ReplyToMessage.ForumTopicCreated != nil {
		_, err := utils.TgReplyTextByContext(b, c, usageString, nil, false)
		return err
	}
	waChatID := args[1]

	var (
		msgToForward                   = c.EffectiveMessage.ReplyToMessage
		msgToReplyTo  *gotgbot.Message = nil
		stanzaID                       = ""
		participantID                  = ""
	)

	waChatJID, ok := utils.WaParseJID(waChatID)
	if !ok {
		_, err := utils.TgReplyTextByContext(b, c, "Provided JID is not valid", nil, false)
		return err
	}

	return utils.TgSendToWhatsApp(b, c, msgToForward, msgToReplyTo, waChatJID, participantID, stanzaID, "", false)
}

func RevokeCommandHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	usageString := "Usage : Reply to a message, <code>/revoke</code>"

	if c.EffectiveMessage.ReplyToMessage == nil || c.EffectiveMessage.ReplyToMessage.ForumTopicClosed != nil {
		_, err := utils.TgReplyTextByContext(b, c, usageString, nil, false)
		return err
	}

	var (
		waClient    = state.State.WhatsAppClient
		msgToRevoke = c.EffectiveMessage.ReplyToMessage
		chatId      = c.EffectiveChat.Id
	)

	waMsgId, _, waChatId, err := database.MsgIdGetWaFromTg(chatId, msgToRevoke.MessageId, msgToRevoke.MessageThreadId)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "failed to retrieve WhatsApp side IDs", err)
	}

	chatJid, _ := utils.WaParseJID(waChatId)
	revokeMessage := waClient.BuildRevoke(chatJid, waTypes.EmptyJID, waMsgId)
	_, err = waClient.SendMessage(context.Background(), chatJid, revokeMessage)
	if err != nil {
		return utils.TgReplyWithErrorByContext(b, c, "failed to revoke message", err)
	}

	_, err = utils.TgReplyTextByContext(b, c, "<i>Successfully revoked</i>", nil, false)
	return err
}

func RevokeCallbackHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	var (
		waClient = state.State.WhatsAppClient
		cq       = c.CallbackQuery
		data     = strings.Split(cq.Data, "_")
	)

	if len(data) == 3 {

		confirmKeyboard := utils.TgMakeRevokeKeyboard(data[1], data[2], true)
		_, _, err := b.EditMessageText("Revoke the message ?", &gotgbot.EditMessageTextOpts{
			ChatId:      c.EffectiveChat.Id,
			MessageId:   c.EffectiveMessage.MessageId,
			ReplyMarkup: *confirmKeyboard,
		})
		cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{
			Text:      "Are you sure ?",
			ShowAlert: false,
		})
		return err

	} else if len(data) == 4 {

		confirmation := data[3]
		if confirmation == "n" {

			revokeKeyboard := utils.TgMakeRevokeKeyboard(data[1], data[2], false)
			_, _, err := b.EditMessageText("Successfully sent", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *revokeKeyboard,
			})
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{
				Text:      "Aborted",
				ShowAlert: true,
			})
			return err

		} else if confirmation == "y" {

			chatJid, _ := utils.WaParseJID(data[2])
			revokeMesssage := waClient.BuildRevoke(chatJid, waTypes.EmptyJID, data[1])
			_, err := waClient.SendMessage(context.Background(), chatJid, revokeMesssage)
			if err != nil {
				_, err = cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{
					Text:      "Failed to send revoke message : " + err.Error(),
					ShowAlert: true,
					CacheTime: 60,
				})
				return err
			} else {
				_, err = cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{
					Text:      "Successfully revoked",
					ShowAlert: true,
					CacheTime: 60,
				})
				b.EditMessageText("<i>Revoked</i>", &gotgbot.EditMessageTextOpts{
					ChatId:    c.EffectiveChat.Id,
					MessageId: c.EffectiveMessage.MessageId,
					ReplyMarkup: gotgbot.InlineKeyboardMarkup{
						InlineKeyboard: [][]gotgbot.InlineKeyboardButton{},
					},
				})
				database.MsgIdDeletePair(c.EffectiveChat.Id, c.EffectiveMessage.MessageId)
				return err
			}

		} else {

			_, err := cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{
				Text:      "Invalid callback query",
				ShowAlert: true,
				CacheTime: 60,
			})
			return err
		}

	} else {

		_, err := cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{
			Text:      "Invalid callback query",
			ShowAlert: true,
			CacheTime: 60,
		})
		return err
	}
}

// aiPendingEntry holds a pending AI response waiting for user confirmation.
type aiPendingEntry struct {
	WaChatJID   string
	AIResponse  string
	StanzaID    string
	ParticipantID string
	IsReply     bool
	TgChatID    int64
	TgThreadID  int64
	StatusMsgID int64
}

var aiPendingStore sync.Map // key: string ID → aiPendingEntry

func aiPendingID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func TgMakeMenuKeyboard() *gotgbot.InlineKeyboardMarkup {
	return &gotgbot.InlineKeyboardMarkup{
		InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
			{
				{Text: "📋 WhatsApp Groups", CallbackData: "menu_groups"},
				{Text: "🔄 Sync Contacts", CallbackData: "menu_sync"},
			},
			{
				{Text: "⚙️ Chat & Threads", CallbackData: "menu_chats"},
				{Text: "🛠️ Tools & Utils", CallbackData: "menu_tools"},
			},
			{
				{Text: "🔌 Restart WhatsApp", CallbackData: "menu_restartwa"},
				{Text: "❓ Help", CallbackData: "menu_help"},
			},
			{
				{Text: "❌ Close Menu", CallbackData: "menu_close"},
			},
		},
	}
}

func MenuCommandHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	keyboard := TgMakeMenuKeyboard()
	_, err := utils.TgReplyTextByContext(b, c, "<b>WaTgBridge Control Panel</b>\n\nChoose an action from the options below:", keyboard, false)
	return err
}

func MenuCallbackHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	var (
		cq   = c.CallbackQuery
		data = cq.Data
	)

	if strings.HasPrefix(data, "menu_linkpage_") {
		pageStr := strings.TrimPrefix(data, "menu_linkpage_")
		var page int
		fmt.Sscanf(pageStr, "%d", &page)

		waClient := state.State.WhatsAppClient
		waGroups, err := waClient.GetJoinedGroups(context.Background())
		if err != nil {
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Failed to fetch groups: " + err.Error(), ShowAlert: true})
			return err
		}

		itemsPerPage := 5
		startIdx := page * itemsPerPage
		if startIdx >= len(waGroups) {
			startIdx = 0
			page = 0
		}
		endIdx := startIdx + itemsPerPage
		if endIdx > len(waGroups) {
			endIdx = len(waGroups)
		}

		var buttons [][]gotgbot.InlineKeyboardButton
		for i := startIdx; i < endIdx; i++ {
			group := waGroups[i]
			displayName := group.Name
			if len(displayName) > 20 {
				displayName = displayName[:17] + "..."
			}

			buttons = append(buttons, []gotgbot.InlineKeyboardButton{{
				Text:         "👥 " + displayName,
				CallbackData: "menu_linkto_" + group.JID.String(),
			}})
		}

		var navRow []gotgbot.InlineKeyboardButton
		if page > 0 {
			navRow = append(navRow, gotgbot.InlineKeyboardButton{
				Text:         "◀️ Prev",
				CallbackData: fmt.Sprintf("menu_linkpage_%d", page-1),
			})
		}
		if endIdx < len(waGroups) {
			navRow = append(navRow, gotgbot.InlineKeyboardButton{
				Text:         "▶️ Next",
				CallbackData: fmt.Sprintf("menu_linkpage_%d", page+1),
			})
		}
		if len(navRow) > 0 {
			buttons = append(buttons, navRow)
		}

		buttons = append(buttons, []gotgbot.InlineKeyboardButton{{
			Text:         "🔙 Back",
			CallbackData: "menu_chats",
		}})

		_, _, err = b.EditMessageText(fmt.Sprintf("<b>Link to WhatsApp Chat</b> (Page %d/%d)\n\nSelect a group chat to link with the current thread:", page+1, (len(waGroups)+itemsPerPage-1)/itemsPerPage), &gotgbot.EditMessageTextOpts{
			ChatId:      c.EffectiveChat.Id,
			MessageId:   c.EffectiveMessage.MessageId,
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: buttons},
			ParseMode:   "HTML",
		})
		cq.Answer(b, nil)
		return err

	} else if strings.HasPrefix(data, "menu_linkto_") {
		targetJIDStr := strings.TrimPrefix(data, "menu_linkto_")
		targetJID, ok := utils.WaParseJID(targetJIDStr)
		if !ok {
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Invalid JID selected", ShowAlert: true})
			return nil
		}

		err := database.ChatThreadAddNewPair(targetJID.String(), c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId)
		if err != nil {
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Failed to save link: " + err.Error(), ShowAlert: true})
			return err
		}

		groupName := utils.WaGetGroupName(targetJID)
		cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Successfully linked to: " + groupName})

		if c.EffectiveMessage.MessageThreadId != 0 {
			_, _ = b.EditForumTopic(c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId, &gotgbot.EditForumTopicOpts{
				Name: groupName,
			})
		}

		c.CallbackQuery.Data = "menu_chats"
		return MenuCallbackHandler(b, c)

	} else {
		switch data {
		case "menu_main":
			keyboard := TgMakeMenuKeyboard()
			_, _, err := b.EditMessageText("<b>WaTgBridge Control Panel</b>\n\nChoose an action from the options below:", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *keyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_groups":
			waClient := state.State.WhatsAppClient
			waGroups, err := waClient.GetJoinedGroups(context.Background())
			if err != nil {
				cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Failed to fetch groups", ShowAlert: true})
				return err
			}

			outputString := "<b>WhatsApp Groups:</b>\n\n"
			for groupNum, group := range waGroups {
				outputString += fmt.Sprintf("%v. %s [ <code>%s</code> ]\n",
					groupNum+1, html.EscapeString(group.Name),
					html.EscapeString(group.JID.String()))
				if len(outputString) >= 1500 {
					outputString += "\n<i>...list truncated, too many groups...</i>"
					break
				}
			}

			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_main",
				}}},
			}

			_, _, err = b.EditMessageText(outputString, &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_sync":
			waClient := state.State.WhatsAppClient
			err := waClient.SendPresence(context.Background(), waTypes.PresenceAvailable)
			if err != nil {
				cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Sync failed", ShowAlert: true})
				return err
			}
			go func() {
				contacts, err := waClient.Store.Contacts.GetAllContacts(context.Background())
				if err == nil {
					database.ContactNameBulkAddOrUpdate(contacts)
				}
			}()

			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_main",
				}}},
			}

			_, _, err = b.EditMessageText("🔄 <b>Contacts synchronization started in background...</b>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Sync started"})
			return err

		case "menu_chats":
			waChatID, err := database.ChatThreadGetWaFromTg(c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId)
			if err != nil {
				cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Database error", ShowAlert: true})
				return err
			}

			var text string
			var keyboard *gotgbot.InlineKeyboardMarkup

			if waChatID != "" {
				waChatJID, _ := utils.WaParseJID(waChatID)
				chatName := waChatID
				if waChatJID.Server == waTypes.GroupServer {
					chatName = utils.WaGetGroupName(waChatJID)
				} else {
					chatName = utils.WaGetContactName(waChatJID)
				}

				text = fmt.Sprintf("<b>Chat & Thread Management</b>\n\nStatus: 🔗 <b>Linked</b>\nWhatsApp Chat: <code>%s</code>\nName: <b>%s</b>\n\n<i>Configure or remove the mapping for the current thread.</i>", html.EscapeString(waChatID), html.EscapeString(chatName))
				keyboard = &gotgbot.InlineKeyboardMarkup{
					InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
						{
							{Text: "🔗 Unlink Current Thread", CallbackData: "menu_unlink_current"},
						},
						{
							{Text: "📁 Sync Topic Names (All)", CallbackData: "menu_sync_topics"},
						},
						{
							{Text: "🔙 Back", CallbackData: "menu_main"},
						},
					},
				}
			} else {
				if c.EffectiveMessage.MessageThreadId == 0 {
					text = "<b>Chat & Thread Management</b>\n\nStatus: ❌ <b>Unlinked</b>\n\n<i>This is the main chat. Mapping a WhatsApp chat is only supported inside individual forum topics (threads). Open a topic and run /menu there to configure it.</i>"
					keyboard = &gotgbot.InlineKeyboardMarkup{
						InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
							{
								{Text: "📁 Sync Topic Names (All)", CallbackData: "menu_sync_topics"},
							},
							{
								{Text: "🔙 Back", CallbackData: "menu_main"},
							},
						},
					}
				} else {
					text = "<b>Chat & Thread Management</b>\n\nStatus: ❌ <b>Unlinked</b>\n\n<i>This thread is not mapped to any WhatsApp chat. You can link it using the button below.</i>"
					keyboard = &gotgbot.InlineKeyboardMarkup{
						InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
							{
								{Text: "📌 Link to WhatsApp Chat", CallbackData: "menu_linkpage_0"},
							},
							{
								{Text: "🔙 Back", CallbackData: "menu_main"},
							},
						},
					}
				}
			}

			_, _, err = b.EditMessageText(text, &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *keyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_unlink_current":
			err := database.ChatThreadDropPairByTg(c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId)
			if err != nil {
				cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Failed to unlink: " + err.Error(), ShowAlert: true})
				return err
			}
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Thread successfully unlinked!"})
			c.CallbackQuery.Data = "menu_chats"
			return MenuCallbackHandler(b, c)

		case "menu_tools":
			keyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
					{
						{Text: "🔍 Find Contact JID", CallbackData: "menu_exp_findcontact"},
						{Text: "🖼️ Get Profile Picture", CallbackData: "menu_exp_getprofilepicture"},
					},
					{
						{Text: "🚫 Block User", CallbackData: "menu_exp_block"},
						{Text: "🟢 Unblock User", CallbackData: "menu_exp_unblock"},
					},
					{
						{Text: "🗑️ Clear Pair History", CallbackData: "menu_clearpair_confirm"},
						{Text: "🔄 Update & Restart", CallbackData: "menu_update_confirm"},
					},
					{
						{Text: "🔙 Back", CallbackData: "menu_main"},
					},
				},
			}
			_, _, err := b.EditMessageText("<b>Tools & Utilities</b>\n\nGeneral options and tools for managing the bridge:", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *keyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_restartwa":
			confirmKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
					{{
						Text:         "Yes, Restart",
						CallbackData: "menu_restartwa_confirm",
					}},
					{{
						Text:         "No, Cancel",
						CallbackData: "menu_main",
					}},
				},
			}

			_, _, err := b.EditMessageText("<b>Are you sure you want to restart the WhatsApp connection?</b>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *confirmKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_restartwa_confirm":
			waClient := state.State.WhatsAppClient
			waClient.Disconnect()
			err := waClient.Connect()
			
			var statusText string
			if err != nil {
				statusText = "❌ <b>Failed to restart WhatsApp connection:</b>\n" + html.EscapeString(err.Error())
			} else {
				statusText = "✅ <b>Successfully restarted WhatsApp connection!</b>"
			}

			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_main",
				}}},
			}

			_, _, err = b.EditMessageText(statusText, &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Restart action processed"})
			return err

		case "menu_sync_topics":
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Syncing topic names..."})
			b.EditMessageText("🔄 <b>Syncing topic names...</b>", &gotgbot.EditMessageTextOpts{
				ChatId:    c.EffectiveChat.Id,
				MessageId: c.EffectiveMessage.MessageId,
				ParseMode: "HTML",
			})
			err := SyncTopicNamesHandler(b, c)
			
			var statusText string
			if err != nil {
				statusText = "❌ <b>Failed to sync topic names:</b>\n" + html.EscapeString(err.Error())
			} else {
				statusText = "✅ <b>Successfully synced topic names!</b>"
			}

			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_chats",
				}}},
			}

			b.EditMessageText(statusText, &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			return err

		case "menu_clearpair_confirm":
			confirmKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
					{{
						Text:         "Yes, Clear",
						CallbackData: "menu_clearpair_action",
					}},
					{{
						Text:         "No, Cancel",
						CallbackData: "menu_tools",
					}},
				},
			}

			_, _, err := b.EditMessageText("<b>Are you sure you want to clear message pairs history?</b>\n<i>This will unlink replies of older bridged messages.</i>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *confirmKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_clearpair_action":
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Clearing history..."})
			b.EditMessageText("🗑️ <b>Clearing stored message pairs...</b>", &gotgbot.EditMessageTextOpts{
				ChatId:    c.EffectiveChat.Id,
				MessageId: c.EffectiveMessage.MessageId,
				ParseMode: "HTML",
			})
			err := ClearMessageIdPairsHistoryHandler(b, c)

			var statusText string
			if err != nil {
				statusText = "❌ <b>Failed to clear pairs:</b>\n" + html.EscapeString(err.Error())
			} else {
				statusText = "✅ <b>Successfully cleared message pair history!</b>"
			}

			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_tools",
				}}},
			}

			b.EditMessageText(statusText, &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			return err

		case "menu_update_confirm":
			confirmKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
					{{
						Text:         "Yes, Update & Restart",
						CallbackData: "menu_update_action",
					}},
					{{
						Text:         "No, Cancel",
						CallbackData: "menu_tools",
					}},
				},
			}

			_, _, err := b.EditMessageText("<b>Are you sure you want to update the bot from GitHub and restart?</b>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *confirmKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_update_action":
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Updating..."})
			b.EditMessageText("🔄 <b>Checking for updates and rebuilding...</b>", &gotgbot.EditMessageTextOpts{
				ChatId:    c.EffectiveChat.Id,
				MessageId: c.EffectiveMessage.MessageId,
				ParseMode: "HTML",
			})
			return UpdateAndRestartHandler(b, c)

		case "menu_exp_settargetgroupchat":
			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_chats",
				}}},
			}
			_, _, err := b.EditMessageText("<b>📌 Link Group Chat</b>\n\nTo link the current Telegram topic to a WhatsApp group chat, send this command in this topic:\n\n<code>/settargetgroupchat &lt;whatsapp_group_jid&gt;</code>\n\n<i>Example: /settargetgroupchat 120363042@g.us</i>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_exp_settargetprivatechat":
			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_chats",
				}}},
			}
			_, _, err := b.EditMessageText("<b>👤 Link Private Chat</b>\n\nTo link the current Telegram topic to a WhatsApp private chat, send this command in this topic:\n\n<code>/settargetprivatechat &lt;whatsapp_user_jid&gt;</code>\n\n<i>Example: /settargetprivatechat 393664019966@s.whatsapp.net</i>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_exp_unlinkthread":
			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_chats",
				}}},
			}
			_, _, err := b.EditMessageText("<b>🔗 Unlink Thread</b>\n\nTo unlink the current Telegram topic from its WhatsApp chat, send this command in this topic:\n\n<code>/unlinkthread</code>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_exp_findcontact":
			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_tools",
				}}},
			}
			_, _, err := b.EditMessageText("<b>🔍 Find Contact JID</b>\n\nTo search for a contact JID by name, send:\n\n<code>/findcontact &lt;name&gt;</code>\n\n<i>Example: /findcontact John</i>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_exp_getprofilepicture":
			waChatID, err := database.ChatThreadGetWaFromTg(c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId)
			if err == nil && waChatID != "" {
				cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Fetching profile picture..."})
				b.EditMessageText("🖼️ <b>Fetching profile picture...</b>", &gotgbot.EditMessageTextOpts{
					ChatId:    c.EffectiveChat.Id,
					MessageId: c.EffectiveMessage.MessageId,
					ParseMode: "HTML",
				})

				waClient := state.State.WhatsAppClient
				userJID, _ := utils.WaParseJID(waChatID)

				ppInfo, err := waClient.GetProfilePictureInfo(context.Background(), userJID, &whatsmeow.GetProfilePictureParams{})
				if err != nil {
					backKeyboard := &gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{Text: "🔙 Back", CallbackData: "menu_tools"}}}}
					_, _, _ = b.EditMessageText("❌ <b>Failed to fetch profile picture info from WhatsApp:</b>\n"+html.EscapeString(err.Error()), &gotgbot.EditMessageTextOpts{
						ChatId:      c.EffectiveChat.Id,
						MessageId:   c.EffectiveMessage.MessageId,
						ReplyMarkup: *backKeyboard,
						ParseMode:   "HTML",
					})
					return err
				}

				res, err := http.DefaultClient.Get(ppInfo.URL)
				if err != nil {
					backKeyboard := &gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{Text: "🔙 Back", CallbackData: "menu_tools"}}}}
					_, _, _ = b.EditMessageText("❌ <b>Failed to download photo:</b>\n"+html.EscapeString(err.Error()), &gotgbot.EditMessageTextOpts{
						ChatId:      c.EffectiveChat.Id,
						MessageId:   c.EffectiveMessage.MessageId,
						ReplyMarkup: *backKeyboard,
						ParseMode:   "HTML",
					})
					return err
				}
				defer res.Body.Close()

				imgBytes, err := io.ReadAll(res.Body)
				if err == nil {
					opts := &gotgbot.SendPhotoOpts{}
					if c.EffectiveMessage.MessageThreadId != 0 {
						opts.MessageThreadId = c.EffectiveMessage.MessageThreadId
					}
					_, _ = b.SendPhoto(c.EffectiveChat.Id, &gotgbot.FileReader{Data: bytes.NewReader(imgBytes)}, opts)
				}

				c.CallbackQuery.Data = "menu_tools"
				return MenuCallbackHandler(b, c)
			}

			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_tools",
				}}},
			}
			_, _, err = b.EditMessageText("<b>🖼️ Get Profile Picture</b>\n\nTo fetch the profile picture of a JID, send:\n\n<code>/getprofilepicture &lt;jid&gt;</code>\n\n<i>Example: /getprofilepicture 120363042@g.us</i>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_exp_block":
			waChatID, err := database.ChatThreadGetWaFromTg(c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId)
			if err == nil && waChatID != "" {
				waChatJID, _ := utils.WaParseJID(waChatID)
				if waChatJID.Server == waTypes.GroupServer {
					backKeyboard := &gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{Text: "🔙 Back", CallbackData: "menu_tools"}}}}
					_, _, err = b.EditMessageText("⚠️ <b>Block user is not applicable:</b>\nThis thread is linked to a WhatsApp Group. Only users can be blocked.", &gotgbot.EditMessageTextOpts{
						ChatId:      c.EffectiveChat.Id,
						MessageId:   c.EffectiveMessage.MessageId,
						ReplyMarkup: *backKeyboard,
						ParseMode:   "HTML",
					})
					cq.Answer(b, nil)
					return err
				}

				confirmKeyboard := &gotgbot.InlineKeyboardMarkup{
					InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
						{{Text: "Yes, Block User", CallbackData: "menu_block_action"}},
						{{Text: "No, Cancel", CallbackData: "menu_tools"}},
					},
				}
				chatName := utils.WaGetContactName(waChatJID)
				_, _, err = b.EditMessageText(fmt.Sprintf("<b>Are you sure you want to block this user on WhatsApp?</b>\n\nName: <b>%s</b>\nJID: <code>%s</code>", html.EscapeString(chatName), html.EscapeString(waChatID)), &gotgbot.EditMessageTextOpts{
					ChatId:      c.EffectiveChat.Id,
					MessageId:   c.EffectiveMessage.MessageId,
					ReplyMarkup: *confirmKeyboard,
					ParseMode:   "HTML",
				})
				cq.Answer(b, nil)
				return err
			}

			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_tools",
				}}},
			}
			_, _, err = b.EditMessageText("<b>🚫 Block User</b>\n\nTo block a WhatsApp user, send:\n\n<code>/block &lt;user_jid&gt;</code>\n\n<i>Example: /block 393664019966@s.whatsapp.net</i>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_exp_unblock":
			waChatID, err := database.ChatThreadGetWaFromTg(c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId)
			if err == nil && waChatID != "" {
				waChatJID, _ := utils.WaParseJID(waChatID)
				if waChatJID.Server == waTypes.GroupServer {
					backKeyboard := &gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{Text: "🔙 Back", CallbackData: "menu_tools"}}}}
					_, _, err = b.EditMessageText("⚠️ <b>Unblock user is not applicable:</b>\nThis thread is linked to a WhatsApp Group. Group chats cannot be blocked/unblocked.", &gotgbot.EditMessageTextOpts{
						ChatId:      c.EffectiveChat.Id,
						MessageId:   c.EffectiveMessage.MessageId,
						ReplyMarkup: *backKeyboard,
						ParseMode:   "HTML",
					})
					cq.Answer(b, nil)
					return err
				}

				confirmKeyboard := &gotgbot.InlineKeyboardMarkup{
					InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
						{{Text: "Yes, Unblock User", CallbackData: "menu_unblock_action"}},
						{{Text: "No, Cancel", CallbackData: "menu_tools"}},
					},
				}
				chatName := utils.WaGetContactName(waChatJID)
				_, _, err = b.EditMessageText(fmt.Sprintf("<b>Are you sure you want to unblock this user on WhatsApp?</b>\n\nName: <b>%s</b>\nJID: <code>%s</code>", html.EscapeString(chatName), html.EscapeString(waChatID)), &gotgbot.EditMessageTextOpts{
					ChatId:      c.EffectiveChat.Id,
					MessageId:   c.EffectiveMessage.MessageId,
					ReplyMarkup: *confirmKeyboard,
					ParseMode:   "HTML",
				})
				cq.Answer(b, nil)
				return err
			}

			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_tools",
				}}},
			}
			_, _, err = b.EditMessageText("<b>🟢 Unblock User</b>\n\nTo unblock a WhatsApp user, send:\n\n<code>/unblock &lt;user_jid&gt;</code>\n\n<i>Example: /unblock 393664019966@s.whatsapp.net</i>", &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_block_action":
			waChatID, err := database.ChatThreadGetWaFromTg(c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId)
			if err != nil || waChatID == "" {
				cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Not linked or error", ShowAlert: true})
				return err
			}
			jid, _ := utils.WaParseJID(waChatID)
			_, err = state.State.WhatsAppClient.UpdateBlocklist(context.Background(), jid, events.BlocklistChangeActionBlock)
			if err != nil {
				backKeyboard := &gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{Text: "🔙 Back", CallbackData: "menu_tools"}}}}
				_, _, _ = b.EditMessageText("❌ <b>Failed to block user:</b>\n"+html.EscapeString(err.Error()), &gotgbot.EditMessageTextOpts{
					ChatId:      c.EffectiveChat.Id,
					MessageId:   c.EffectiveMessage.MessageId,
					ReplyMarkup: *backKeyboard,
					ParseMode:   "HTML",
				})
				return err
			}
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Successfully blocked!"})
			c.CallbackQuery.Data = "menu_tools"
			return MenuCallbackHandler(b, c)

		case "menu_unblock_action":
			waChatID, err := database.ChatThreadGetWaFromTg(c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId)
			if err != nil || waChatID == "" {
				cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Not linked or error", ShowAlert: true})
				return err
			}
			jid, _ := utils.WaParseJID(waChatID)
			_, err = state.State.WhatsAppClient.UpdateBlocklist(context.Background(), jid, events.BlocklistChangeActionUnblock)
			if err != nil {
				backKeyboard := &gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{Text: "🔙 Back", CallbackData: "menu_tools"}}}}
				_, _, _ = b.EditMessageText("❌ <b>Failed to unblock user:</b>\n"+html.EscapeString(err.Error()), &gotgbot.EditMessageTextOpts{
					ChatId:      c.EffectiveChat.Id,
					MessageId:   c.EffectiveMessage.MessageId,
					ReplyMarkup: *backKeyboard,
					ParseMode:   "HTML",
				})
				return err
			}
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Successfully unblocked!"})
			c.CallbackQuery.Data = "menu_tools"
			return MenuCallbackHandler(b, c)

		case "menu_help":
			helpText := "<b>Available Commands:</b>\n"
			for _, cmd := range commands {
				if cmd.description != "" {
					helpText += fmt.Sprintf("/%s - %s\n", cmd.command.Command, html.EscapeString(cmd.description))
				}
			}

			backKeyboard := &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{{
					Text:         "🔙 Back",
					CallbackData: "menu_main",
				}}},
			}

			_, _, err := b.EditMessageText(helpText, &gotgbot.EditMessageTextOpts{
				ChatId:      c.EffectiveChat.Id,
				MessageId:   c.EffectiveMessage.MessageId,
				ReplyMarkup: *backKeyboard,
				ParseMode:   "HTML",
			})
			cq.Answer(b, nil)
			return err

		case "menu_close":
			_, err := b.DeleteMessage(c.EffectiveChat.Id, c.EffectiveMessage.MessageId, &gotgbot.DeleteMessageOpts{})
			cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Menu closed"})
			return err
		}
	}

	return nil
}

func AIChatHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	cfg := state.State.Config
	if !cfg.Gemini.Enabled || cfg.Gemini.APIKey == "" {
		_, err := utils.TgReplyTextByContext(b, c, "Gemini AI is not enabled or api_key is missing in config.", nil, false)
		return err
	}

	args := c.Args()
	promptText := ""
	if len(args) > 1 {
		promptText = strings.Join(args[1:], " ")
	}

	var (
		msgToForward = c.EffectiveMessage
		msgToReplyTo = c.EffectiveMessage.ReplyToMessage
		waClient     = state.State.WhatsAppClient
	)

	var stanzaID, participantID, waChatID string
	var err error

	if msgToReplyTo != nil && msgToReplyTo.ForumTopicCreated == nil {
		stanzaID, participantID, waChatID, err = database.MsgIdGetWaFromTg(c.EffectiveChat.Id, msgToReplyTo.MessageId, msgToForward.MessageThreadId)
		if err == nil && waChatID == waClient.Store.ID.String() {
			waChatID = participantID
		}
	} else {
		waChatID, err = database.ChatThreadGetWaFromTg(c.EffectiveChat.Id, c.EffectiveMessage.MessageThreadId)
	}

	var finalPrompt string
	if msgToReplyTo != nil && msgToReplyTo.Text != "" {
		finalPrompt = fmt.Sprintf("System Instructions: %s\n\nOriginal message received: \"%s\"\nUser's prompt/instruction: \"%s\"", cfg.Gemini.SystemPrompt, msgToReplyTo.Text, promptText)
	} else if promptText != "" {
		finalPrompt = fmt.Sprintf("System Instructions: %s\n\nPrompt: \"%s\"", cfg.Gemini.SystemPrompt, promptText)
	} else {
		_, err := utils.TgReplyTextByContext(b, c, "Please provide a prompt or reply to a message with <code>/ai <your instruction></code>", nil, false)
		return err
	}

	statusMsg, err := utils.TgReplyTextByContext(b, c, "🤖 <i>Gemini is thinking...</i>", nil, false)
	if err != nil {
		return err
	}

	aiResponse, err := utils.CallGemini(cfg.Gemini.APIKey, finalPrompt)
	if err != nil {
		b.EditMessageText(fmt.Sprintf("❌ <b>Error calling Gemini:</b>\n%s", html.EscapeString(err.Error())), &gotgbot.EditMessageTextOpts{
			ChatId:    c.EffectiveChat.Id,
			MessageId: statusMsg.MessageId,
			ParseMode: "HTML",
		})
		return err
	}

	_, ok := utils.WaParseJID(waChatID)
	if waChatID != "" && ok {
		// Save the pending response and show confirmation buttons
		pendingID := aiPendingID()
		aiPendingStore.Store(pendingID, aiPendingEntry{
			WaChatJID:     waChatID,
			AIResponse:    aiResponse,
			StanzaID:      stanzaID,
			ParticipantID: participantID,
			IsReply:       stanzaID != "",
			TgChatID:      c.EffectiveChat.Id,
			TgThreadID:    statusMsg.MessageThreadId,
			StatusMsgID:   statusMsg.MessageId,
		})

		// Auto-expire after 5 minutes to avoid memory leaks
		go func(id string) {
			time.Sleep(5 * time.Minute)
			aiPendingStore.Delete(id)
		}(pendingID)

		confirmKeyboard := &gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
				{
					{Text: "✅ Send to WhatsApp", CallbackData: "ai_send_" + pendingID},
					{Text: "❌ Discard", CallbackData: "ai_discard_" + pendingID},
				},
			},
		}

		b.EditMessageText(fmt.Sprintf("🤖 <b>AI response ready:</b>\n\n%s\n\n<i>Send this message to WhatsApp?</i>", html.EscapeString(aiResponse)), &gotgbot.EditMessageTextOpts{
			ChatId:      c.EffectiveChat.Id,
			MessageId:   statusMsg.MessageId,
			ReplyMarkup: *confirmKeyboard,
			ParseMode:   "HTML",
		})
		return nil
	}

	// No WhatsApp chat linked — just show the response in Telegram
	b.EditMessageText(fmt.Sprintf("🤖 <b>AI response:</b>\n\n%s", html.EscapeString(aiResponse)), &gotgbot.EditMessageTextOpts{
		ChatId:    c.EffectiveChat.Id,
		MessageId: statusMsg.MessageId,
		ParseMode: "HTML",
	})
	return nil
}

func AIPendingCallbackHandler(b *gotgbot.Bot, c *ext.Context) error {
	if !utils.TgUpdateIsAuthorized(b, c) {
		return nil
	}

	cq := c.CallbackQuery
	data := cq.Data

	var pendingID, action string
	if strings.HasPrefix(data, "ai_send_") {
		pendingID = strings.TrimPrefix(data, "ai_send_")
		action = "send"
	} else if strings.HasPrefix(data, "ai_discard_") {
		pendingID = strings.TrimPrefix(data, "ai_discard_")
		action = "discard"
	} else {
		return nil
	}

	raw, exists := aiPendingStore.LoadAndDelete(pendingID)
	if !exists {
		cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "This response has already been handled or expired.", ShowAlert: true})
		b.EditMessageText("⏳ <i>This response has already been handled or expired.</i>", &gotgbot.EditMessageTextOpts{
			ChatId:    c.EffectiveChat.Id,
			MessageId: c.EffectiveMessage.MessageId,
			ParseMode: "HTML",
		})
		return nil
	}

	entry := raw.(aiPendingEntry)

	if action == "discard" {
		cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Response discarded."})
		b.EditMessageText("🗑️ <i>AI response discarded.</i>", &gotgbot.EditMessageTextOpts{
			ChatId:    c.EffectiveChat.Id,
			MessageId: c.EffectiveMessage.MessageId,
			ParseMode: "HTML",
		})
		return nil
	}

	// action == "send"
	cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Sending to WhatsApp..."})
	b.EditMessageText("📤 <i>Sending to WhatsApp...</i>", &gotgbot.EditMessageTextOpts{
		ChatId:    c.EffectiveChat.Id,
		MessageId: c.EffectiveMessage.MessageId,
		ParseMode: "HTML",
	})

	waClient := state.State.WhatsAppClient
	waChatJID, _ := utils.WaParseJID(entry.WaChatJID)
	var quotedMsg *waE2E.Message

	sendResp, err := utils.WaSendText(waChatJID, entry.AIResponse, entry.StanzaID, entry.ParticipantID, quotedMsg, entry.IsReply)
	if err != nil {
		b.EditMessageText(fmt.Sprintf("🤖 <b>AI response generated:</b>\n\n%s\n\n❌ <i>Failed to send to WhatsApp: %s</i>",
			html.EscapeString(entry.AIResponse), html.EscapeString(err.Error())), &gotgbot.EditMessageTextOpts{
			ChatId:    c.EffectiveChat.Id,
			MessageId: c.EffectiveMessage.MessageId,
			ParseMode: "HTML",
		})
		return err
	}

	b.EditMessageText(fmt.Sprintf("✅ <b>AI response sent to WhatsApp:</b>\n\n%s", html.EscapeString(entry.AIResponse)), &gotgbot.EditMessageTextOpts{
		ChatId:    c.EffectiveChat.Id,
		MessageId: c.EffectiveMessage.MessageId,
		ParseMode: "HTML",
	})

	database.MsgIdAddNewPair(sendResp.ID, waClient.Store.ID.String(), waChatJID.String(), c.EffectiveChat.Id, c.EffectiveMessage.MessageId, entry.TgThreadID)
	return nil
}

