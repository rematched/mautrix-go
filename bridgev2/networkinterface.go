// Copyright (c) 2024 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/configupgrade"
	"go.mau.fi/util/ptr"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/mediaproxy"
)

type ConvertedMessagePart struct {
	ID         networkid.PartID
	Type       event.Type
	Content    *event.MessageEventContent
	Extra      map[string]any
	DBMetadata any
	DontBridge bool
}

func (cmp *ConvertedMessagePart) ToEditPart(part *database.Message) *ConvertedEditPart {
	if cmp == nil {
		return nil
	}
	if cmp.DBMetadata != nil {
		merger, ok := part.Metadata.(database.MetaMerger)
		if ok {
			merger.CopyFrom(cmp.DBMetadata)
		} else {
			part.Metadata = cmp.DBMetadata
		}
	}
	return &ConvertedEditPart{
		Part:       part,
		Type:       cmp.Type,
		Content:    cmp.Content,
		Extra:      cmp.Extra,
		DontBridge: cmp.DontBridge,
	}
}

// EventSender represents a specific user in a chat.
type EventSender struct {
	// If IsFromMe is true, the UserLogin who the event was received through is used as the sender.
	// Double puppeting will be used if available.
	IsFromMe bool
	// SenderLogin is the ID of the UserLogin who sent the event. This may be different from the
	// login the event was received through. It is used to ensure double puppeting can still be
	// used even if the event is received through another login.
	SenderLogin networkid.UserLoginID
	// Sender is the remote user ID of the user who sent the event.
	// For new events, this will not be used for double puppeting.
	//
	// However, in the member list, [ChatMemberList.CheckAllLogins] can be specified to go through every login
	// and call [NetworkAPI.IsThisUser] to check if this ID belongs to that login. This method is not recommended,
	// it is better to fill the IsFromMe and SenderLogin fields appropriately.
	Sender networkid.UserID

	// ForceDMUser can be set if the event should be sent as the DM user even if the Sender is different.
	// This only applies in DM rooms where [database.Portal.OtherUserID] is set and is ignored if IsFromMe is true.
	// A warning will be logged if the sender is overridden due to this flag.
	ForceDMUser bool
}

type ConvertedMessage struct {
	ReplyTo    *networkid.MessageOptionalPartID
	ThreadRoot *networkid.MessageID
	Parts      []*ConvertedMessagePart
	Disappear  database.DisappearingSetting
}

func MergeCaption(textPart, mediaPart *ConvertedMessagePart) *ConvertedMessagePart {
	if textPart == nil {
		return mediaPart
	} else if mediaPart == nil {
		return textPart
	}
	mediaPart = ptr.Clone(mediaPart)
	if mediaPart.Content.MsgType == event.MsgNotice || (mediaPart.Content.Body != "" && mediaPart.Content.FileName != "" && mediaPart.Content.Body != mediaPart.Content.FileName) {
		textPart = ptr.Clone(textPart)
		textPart.Content.EnsureHasHTML()
		mediaPart.Content.EnsureHasHTML()
		mediaPart.Content.Body += "\n\n" + textPart.Content.Body
		mediaPart.Content.FormattedBody += "<br><br>" + textPart.Content.FormattedBody
	} else {
		mediaPart.Content.FileName = mediaPart.Content.Body
		mediaPart.Content.Body = textPart.Content.Body
		mediaPart.Content.Format = textPart.Content.Format
		mediaPart.Content.FormattedBody = textPart.Content.FormattedBody
	}
	if metaMerger, ok := mediaPart.DBMetadata.(database.MetaMerger); ok {
		metaMerger.CopyFrom(textPart.DBMetadata)
	}
	mediaPart.ID = textPart.ID
	return mediaPart
}

func (cm *ConvertedMessage) MergeCaption() bool {
	if len(cm.Parts) != 2 {
		return false
	}
	textPart, mediaPart := cm.Parts[0], cm.Parts[1]
	if textPart.Content.MsgType.IsMedia() {
		textPart, mediaPart = mediaPart, textPart
	}
	if (!mediaPart.Content.MsgType.IsMedia() && mediaPart.Content.MsgType != event.MsgNotice) || textPart.Content.MsgType != event.MsgText {
		return false
	}
	merged := MergeCaption(textPart, mediaPart)
	if merged != nil {
		cm.Parts = []*ConvertedMessagePart{merged}
		return true
	}
	return false
}

type ConvertedEditPart struct {
	Part *database.Message

	Type event.Type
	// The Content and Extra fields will be put inside `m.new_content` automatically.
	// SetEdit must NOT be called by the network connector.
	Content *event.MessageEventContent
	Extra   map[string]any
	// TopLevelExtra can be used to specify custom fields at the top level of the content rather than inside `m.new_content`.
	TopLevelExtra map[string]any

	DontBridge bool
}

type ConvertedEdit struct {
	ModifiedParts []*ConvertedEditPart
	DeletedParts  []*database.Message
	// Warning: added parts will be sent at the end of the room.
	// If other messages have been sent after the message being edited,
	// these new parts will not be next to the existing parts.
	AddedParts *ConvertedMessage
}

// BridgeName contains information about the network that a connector bridges to.
type BridgeName struct {
	// The displayname of the network, e.g. `Discord`
	DisplayName string `json:"displayname"`
	// The URL to the website of the network, e.g. `https://discord.com`
	NetworkURL string `json:"network_url"`
	// The icon of the network as a mxc:// URI
	NetworkIcon id.ContentURIString `json:"network_icon"`
	// An identifier uniquely identifying the network, e.g. `discord`
	NetworkID string `json:"network_id"`
	// An identifier uniquely identifying the bridge software.
	// The Go import path is a good choice here (e.g. github.com/octocat/discordbridge)
	BeeperBridgeType string `json:"beeper_bridge_type"`
	// The default appservice port to use in the example config, defaults to 8080 if unset
	// Official mautrix bridges will use ports defined in https://mau.fi/ports
	DefaultPort uint16 `json:"default_port,omitempty"`
	// The default command prefix to use in the example config, defaults to NetworkID if unset. Must include the ! prefix.
	DefaultCommandPrefix string `json:"default_command_prefix,omitempty"`
}

func (bn BridgeName) AsBridgeInfoSection() event.BridgeInfoSection {
	return event.BridgeInfoSection{
		ID:          bn.BeeperBridgeType,
		DisplayName: bn.DisplayName,
		AvatarURL:   bn.NetworkIcon,
		ExternalURL: bn.NetworkURL,
	}
}

// NetworkConnector is the main interface that a network connector must implement.
type NetworkConnector interface {
	// Init is called when the bridge is initialized. The connector should store the bridge instance for later use.
	// This should not do any network calls or other blocking operations.
	Init(*Bridge)
	// Start is called when the bridge is starting.
	// The connector should do any non-user-specific startup actions necessary.
	// User logins will be loaded separately, so the connector should not load them here.
	Start(context.Context) error

	// GetName returns the name of the bridge and some additional metadata,
	// which is used to fill `m.bridge` events among other things.
	//
	// The first call happens *before* the config is loaded, because the data here is also used to
	// fill parts of the example config (like the default username template and bot localpart).
	// The output can still be adjusted based on config variables, but the function must have
	// default values when called without a config.
	GetName() BridgeName
	// GetDBMetaTypes returns struct types that are used to store connector-specific metadata in various tables.
	// All fields are optional. If a field isn't provided, then the corresponding table will have no custom metadata.
	// This will be called before Init, it should have a hardcoded response.
	GetDBMetaTypes() database.MetaTypes
	// GetCapabilities returns the general capabilities of the network connector.
	// Note that most capabilities are scoped to rooms and are returned by [NetworkAPI.GetCapabilities] instead.
	GetCapabilities() *NetworkGeneralCapabilities
	// GetConfig returns all the parts of the network connector's config file. Specifically:
	// - example: a string containing an example config file
	// - data: an interface to unmarshal the actual config into
	// - upgrader: a config upgrader to ensure all fields are present and to do any migrations from old configs
	GetConfig() (example string, data any, upgrader configupgrade.Upgrader)

	// LoadUserLogin is called when a UserLogin is loaded from the database in order to fill the [UserLogin.Client] field.
	//
	// This is called within the bridge's global cache lock, so it must not do any slow operations,
	// such as connecting to the network. Instead, connecting should happen when [NetworkAPI.Connect] is called later.
	LoadUserLogin(ctx context.Context, login *UserLogin) error

	// GetLoginFlows returns a list of login flows that the network supports.
	GetLoginFlows() []LoginFlow
	// CreateLogin is called when a user wants to log in to the network.
	//
	// This should generally not do any work, it should just return a LoginProcess that remembers
	// the user and will execute the requested flow. The actual work should start when [LoginProcess.Start] is called.
	CreateLogin(ctx context.Context, user *User, flowID string) (LoginProcess, error)
}

var ErrDirectMediaNotEnabled = errors.New("direct media is not enabled")

// DirectMediableNetwork is an optional interface that network connectors can implement to support direct media access.
//
// If the Matrix connector has direct media enabled, SetUseDirectMedia will be called
// before the Start method of the network connector. Download will then be called
// whenever someone wants to download a direct media `mxc://` URI which was generated
// by calling GenerateContentURI on the Matrix connector.
type DirectMediableNetwork interface {
	NetworkConnector
	SetUseDirectMedia()
	Download(ctx context.Context, mediaID networkid.MediaID) (mediaproxy.GetMediaResponse, error)
}

// ConfigValidatingNetwork is an optional interface that network connectors can implement to validate config fields
// before the bridge is started.
//
// When the ValidateConfig method is called, the config data will already be unmarshaled into the
// object returned by [NetworkConnector.GetConfig].
//
// This mechanism is usually used to refuse bridge startup if a mandatory field has an invalid value.
type ConfigValidatingNetwork interface {
	NetworkConnector
	ValidateConfig() error
}

// MaxFileSizeingNetwork is an optional interface that network connectors can implement
// to find out the maximum file size that can be uploaded to Matrix.
//
// The SetMaxFileSize will be called asynchronously soon after startup.
// Before the function is called, the connector may assume a default limit of 50 MiB.
type MaxFileSizeingNetwork interface {
	NetworkConnector
	SetMaxFileSize(maxSize int64)
}

type MatrixMessageResponse struct {
	DB *database.Message

	Pending    networkid.TransactionID
	HandleEcho func(RemoteMessage, *database.Message) (bool, error)
}

type FileRestriction struct {
	MaxSize   int64
	MimeTypes []string
}

type NetworkGeneralCapabilities struct {
	// Does the network connector support disappearing messages?
	// This flag enables the message disappearing loop in the bridge.
	DisappearingMessages bool
	// Should the bridge re-request user info on incoming messages even if the ghost already has info?
	// By default, info is only requested for ghosts with no name, and other updating is left to events.
	AggressiveUpdateInfo bool
}

type NetworkRoomCapabilities struct {
	FormattedText bool
	UserMentions  bool
	RoomMentions  bool

	LocationMessages bool
	Captions         bool
	MaxTextLength    int
	MaxCaptionLength int

	Threads      bool
	Replies      bool
	Edits        bool
	EditMaxCount int
	EditMaxAge   time.Duration
	Deletes      bool
	DeleteMaxAge time.Duration

	DefaultFileRestriction *FileRestriction
	Files                  map[event.MessageType]FileRestriction

	ReadReceipts bool

	Reactions        bool
	ReactionCount    int
	AllowedReactions []string
}

// NetworkAPI is an interface representing a remote network client for a single user login.
//
// Implementations of this interface are stored in [UserLogin.Client].
// The [NetworkConnector.LoadUserLogin] method is responsible for filling the Client field with a NetworkAPI.
type NetworkAPI interface {
	// Connect is called to actually connect to the remote network.
	// If there's no persistent connection, this may just check access token validity, or even do nothing at all.
	Connect(ctx context.Context) error
	// Disconnect should disconnect from the remote network.
	// A clean disconnection is preferred, but it should not take too long.
	Disconnect()
	// IsLoggedIn should return whether the access tokens in this NetworkAPI are valid.
	// This should not do any IO operations, it should only return cached data which is updated elsewhere.
	IsLoggedIn() bool
	// LogoutRemote should invalidate the access tokens in this NetworkAPI if possible
	// and disconnect from the remote network.
	LogoutRemote(ctx context.Context)

	// IsThisUser should return whether the given remote network user ID is the same as this login.
	// This is used when the bridge wants to convert a user login ID to a user ID.
	IsThisUser(ctx context.Context, userID networkid.UserID) bool
	// GetChatInfo returns info for a given chat. Any fields that are nil will be ignored and not processed at all,
	// while empty strings will change the relevant value in the room to be an empty string.
	// For example, a nil name will mean the room name is not changed, while an empty string name will remove the name.
	GetChatInfo(ctx context.Context, portal *Portal) (*ChatInfo, error)
	// GetUserInfo returns info for a given user. Like chat info, fields can be nil to skip them.
	GetUserInfo(ctx context.Context, ghost *Ghost) (*UserInfo, error)
	// GetCapabilities returns the bridging capabilities in a given room.
	// This can simply return a static list if the remote network has no per-chat capability differences,
	// but all calls will include the portal, because some networks do have per-chat differences.
	GetCapabilities(ctx context.Context, portal *Portal) *NetworkRoomCapabilities

	// HandleMatrixMessage is called when a message is sent from Matrix in an existing portal room.
	// This function should convert the message as appropriate, send it over to the remote network,
	// and return the info so the central bridge can store it in the database.
	//
	// This is only called for normal non-edit messages. For other types of events, see the optional extra interfaces (`XHandlingNetworkAPI`).
	HandleMatrixMessage(ctx context.Context, msg *MatrixMessage) (message *MatrixMessageResponse, err error)
}

// FetchMessagesParams contains the parameters for a message history pagination request.
type FetchMessagesParams struct {
	// The portal to fetch messages in. Always present.
	Portal *Portal
	// When fetching messages inside a thread, the ID of the thread.
	ThreadRoot networkid.MessageID
	// Whether to fetch new messages instead of old ones.
	Forward bool
	// The oldest known message in the thread or the portal. If Forward is true, this is the newest known message instead.
	// If the portal doesn't have any bridged messages, this will be nil.
	AnchorMessage *database.Message
	// The cursor returned by the previous call to FetchMessages with the same portal and thread root.
	// This will not be present in Forward calls.
	Cursor networkid.PaginationCursor
	// The preferred number of messages to return. The returned batch can be bigger or smaller
	// without any side effects, but the network connector should aim for this number.
	Count int
}

// BackfillReaction is an individual reaction to a message in a history pagination request.
//
// The target message is always the BackfillMessage that contains this item.
// Optionally, the reaction can target a specific part by specifying TargetPart.
// If not specified, the first part (sorted lexicographically) is targeted.
type BackfillReaction struct {
	// Optional part of the message that the reaction targets.
	// If nil, the reaction targets the first part of the message.
	TargetPart *networkid.PartID
	// Optional timestamp for the reaction.
	// If unset, the reaction will have a fake timestamp that is slightly after the message timestamp.
	Timestamp time.Time

	Sender       EventSender
	EmojiID      networkid.EmojiID
	Emoji        string
	ExtraContent map[string]any
	DBMetadata   any
}

// BackfillMessage is an individual message in a history pagination request.
type BackfillMessage struct {
	*ConvertedMessage
	Sender    EventSender
	ID        networkid.MessageID
	TxnID     networkid.TransactionID
	Timestamp time.Time
	Reactions []*BackfillReaction

	ShouldBackfillThread bool
	LastThreadMessage    networkid.MessageID
}

// FetchMessagesResponse contains the response for a message history pagination request.
type FetchMessagesResponse struct {
	// The messages to backfill. Messages should always be sorted in chronological order (oldest to newest).
	Messages []*BackfillMessage
	// The next cursor to use for fetching more messages.
	Cursor networkid.PaginationCursor
	// Whether there are more messages that can be backfilled.
	// This field is required. If it is false, FetchMessages will not be called again.
	HasMore bool
	// Whether the batch contains new messages rather than old ones.
	// Cursor, HasMore and the progress fields will be ignored when this is present.
	Forward bool
	// When sending forward backfill (or the first batch in a room), this field can be set
	// to mark the messages as read immediately after backfilling.
	MarkRead bool

	// When HasMore is true, one of the following fields can be set to report backfill progress:

	// Approximate backfill progress as a number between 0 and 1.
	ApproxProgress float64
	// Approximate number of messages remaining that can be backfilled.
	ApproxRemainingCount int
	// Approximate total number of messages in the chat.
	ApproxTotalCount int
}

// BackfillingNetworkAPI is an optional interface that network connectors can implement to support backfilling message history.
type BackfillingNetworkAPI interface {
	NetworkAPI
	FetchMessages(ctx context.Context, fetchParams FetchMessagesParams) (*FetchMessagesResponse, error)
}

// EditHandlingNetworkAPI is an optional interface that network connectors can implement to handle message edits.
type EditHandlingNetworkAPI interface {
	NetworkAPI
	// HandleMatrixEdit is called when a previously bridged message is edited in a portal room.
	// The central bridge module will save the [*database.Message] after this function returns,
	// so the network connector is allowed to mutate the provided object.
	HandleMatrixEdit(ctx context.Context, msg *MatrixEdit) error
}

// ReactionHandlingNetworkAPI is an optional interface that network connectors can implement to handle message reactions.
type ReactionHandlingNetworkAPI interface {
	NetworkAPI
	// PreHandleMatrixReaction is called as the first step of handling a reaction. It returns the emoji ID,
	// sender user ID and max reaction count to allow the central bridge module to de-duplicate the reaction
	// if appropriate.
	PreHandleMatrixReaction(ctx context.Context, msg *MatrixReaction) (MatrixReactionPreResponse, error)
	// HandleMatrixReaction is called after confirming that the reaction is not a duplicate.
	// This is the method that should actually send the reaction to the remote network.
	// The returned [database.Reaction] object may be empty: the central bridge module already has
	// all the required fields and will fill them automatically if they're empty. However, network
	// connectors are allowed to set fields themselves if any extra fields are necessary.
	HandleMatrixReaction(ctx context.Context, msg *MatrixReaction) (reaction *database.Reaction, err error)
	// HandleMatrixReactionRemove is called when a redaction event is received pointing at a previously
	// bridged reaction. The network connector should remove the reaction from the remote network.
	HandleMatrixReactionRemove(ctx context.Context, msg *MatrixReactionRemove) error
}

// RedactionHandlingNetworkAPI is an optional interface that network connectors can implement to handle message deletions.
type RedactionHandlingNetworkAPI interface {
	NetworkAPI
	// HandleMatrixMessageRemove is called when a previously bridged message is deleted in a portal room.
	HandleMatrixMessageRemove(ctx context.Context, msg *MatrixMessageRemove) error
}

// ReadReceiptHandlingNetworkAPI is an optional interface that network connectors can implement to handle read receipts.
type ReadReceiptHandlingNetworkAPI interface {
	NetworkAPI
	// HandleMatrixReadReceipt is called when a read receipt is sent in a portal room.
	// This will be called even if the target message is not a bridged message.
	// Network connectors must gracefully handle [MatrixReadReceipt.ExactMessage] being nil.
	// The exact handling is up to the network connector.
	HandleMatrixReadReceipt(ctx context.Context, msg *MatrixReadReceipt) error
}

// TypingHandlingNetworkAPI is an optional interface that network connectors can implement to handle typing events.
type TypingHandlingNetworkAPI interface {
	NetworkAPI
	// HandleMatrixTyping is called when a user starts typing in a portal room.
	// In the future, the central bridge module will likely get a loop to automatically repeat
	// calls to this function until the user stops typing.
	HandleMatrixTyping(ctx context.Context, msg *MatrixTyping) error
}

type MarkedUnreadHandlingNetworkAPI interface {
	NetworkAPI
	HandleMarkedUnread(ctx context.Context, msg *MatrixMarkedUnread) error
}

type MuteHandlingNetworkAPI interface {
	NetworkAPI
	HandleMute(ctx context.Context, msg *MatrixMute) error
}

type TagHandlingNetworkAPI interface {
	NetworkAPI
	HandleRoomTag(ctx context.Context, msg *MatrixRoomTag) error
}

// RoomNameHandlingNetworkAPI is an optional interface that network connectors can implement to handle room name changes.
type RoomNameHandlingNetworkAPI interface {
	NetworkAPI
	// HandleMatrixRoomName is called when the name of a portal room is changed.
	// This method should update the Name and NameSet fields of the Portal with
	// the new name and return true if the change was successful.
	// If the change is not successful, then the fields should not be updated.
	HandleMatrixRoomName(ctx context.Context, msg *MatrixRoomName) (bool, error)
}

// RoomAvatarHandlingNetworkAPI is an optional interface that network connectors can implement to handle room avatar changes.
type RoomAvatarHandlingNetworkAPI interface {
	NetworkAPI
	// HandleMatrixRoomAvatar is called when the avatar of a portal room is changed.
	// This method should update the AvatarID, AvatarHash and AvatarMXC fields
	// with the new avatar details and return true if the change was successful.
	// If the change is not successful, then the fields should not be updated.
	HandleMatrixRoomAvatar(ctx context.Context, msg *MatrixRoomAvatar) (bool, error)
}

// RoomTopicHandlingNetworkAPI is an optional interface that network connectors can implement to handle room topic changes.
type RoomTopicHandlingNetworkAPI interface {
	NetworkAPI
	// HandleMatrixRoomTopic is called when the topic of a portal room is changed.
	// This method should update the Topic and TopicSet fields of the Portal with
	// the new topic and return true if the change was successful.
	// If the change is not successful, then the fields should not be updated.
	HandleMatrixRoomTopic(ctx context.Context, msg *MatrixRoomTopic) (bool, error)
}

type ResolveIdentifierResponse struct {
	// Ghost is the ghost of the user that the identifier resolves to.
	// This field should be set whenever possible. However, it is not required,
	// and the central bridge module will not try to create a ghost if it is not set.
	Ghost *Ghost

	// UserID is the user ID of the user that the identifier resolves to.
	UserID networkid.UserID
	// UserInfo contains the info of the user that the identifier resolves to.
	// If both this and the Ghost field are set, the central bridge module will
	// automatically update the ghost's info with the data here.
	UserInfo *UserInfo

	// Chat contains info about the direct chat with the resolved user.
	// This field is required when createChat is true in the ResolveIdentifier call,
	// and optional otherwise.
	Chat *CreateChatResponse
}

type CreateChatResponse struct {
	PortalKey networkid.PortalKey
	// Portal and PortalInfo are not required, the caller will fetch them automatically based on PortalKey if necessary.
	Portal     *Portal
	PortalInfo *ChatInfo
}

// IdentifierResolvingNetworkAPI is an optional interface that network connectors can implement to support starting new direct chats.
type IdentifierResolvingNetworkAPI interface {
	NetworkAPI
	// ResolveIdentifier is called when the user wants to start a new chat.
	// This can happen via the `resolve-identifier` or `start-chat` bridge bot commands,
	// or the corresponding provisioning API endpoints.
	ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*ResolveIdentifierResponse, error)
}

// ContactListingNetworkAPI is an optional interface that network connectors can implement to provide the user's contact list.
type ContactListingNetworkAPI interface {
	NetworkAPI
	GetContactList(ctx context.Context) ([]*ResolveIdentifierResponse, error)
}

type UserSearchingNetworkAPI interface {
	IdentifierResolvingNetworkAPI
	SearchUsers(ctx context.Context, query string) ([]*ResolveIdentifierResponse, error)
}

type GroupCreatingNetworkAPI interface {
	IdentifierResolvingNetworkAPI
	CreateGroup(ctx context.Context, name string, users ...networkid.UserID) (*CreateChatResponse, error)
}

type PushType int

func (pt PushType) String() string {
	return pt.GoString()
}

func PushTypeFromString(str string) PushType {
	switch strings.TrimPrefix(strings.ToLower(str), "pushtype") {
	case "web":
		return PushTypeWeb
	case "apns":
		return PushTypeAPNs
	case "fcm":
		return PushTypeFCM
	default:
		return PushTypeUnknown
	}
}

func (pt PushType) GoString() string {
	switch pt {
	case PushTypeUnknown:
		return "PushTypeUnknown"
	case PushTypeWeb:
		return "PushTypeWeb"
	case PushTypeAPNs:
		return "PushTypeAPNs"
	case PushTypeFCM:
		return "PushTypeFCM"
	default:
		return fmt.Sprintf("PushType(%d)", int(pt))
	}
}

const (
	PushTypeUnknown PushType = iota
	PushTypeWeb
	PushTypeAPNs
	PushTypeFCM
)

type WebPushConfig struct {
	VapidKey string `json:"vapid_key"`
}

type FCMPushConfig struct {
	SenderID string `json:"sender_id"`
}

type APNsPushConfig struct {
	BundleID string `json:"bundle_id"`
}

type PushConfig struct {
	Web  *WebPushConfig  `json:"web,omitempty"`
	FCM  *FCMPushConfig  `json:"fcm,omitempty"`
	APNs *APNsPushConfig `json:"apns,omitempty"`
}

type PushableNetworkAPI interface {
	RegisterPushNotifications(ctx context.Context, pushType PushType, token string) error
	GetPushConfigs() *PushConfig
}

type RemoteEventType int

func (ret RemoteEventType) String() string {
	switch ret {
	case RemoteEventUnknown:
		return "RemoteEventUnknown"
	case RemoteEventMessage:
		return "RemoteEventMessage"
	case RemoteEventMessageUpsert:
		return "RemoteEventMessageUpsert"
	case RemoteEventEdit:
		return "RemoteEventEdit"
	case RemoteEventReaction:
		return "RemoteEventReaction"
	case RemoteEventReactionRemove:
		return "RemoteEventReactionRemove"
	case RemoteEventReactionSync:
		return "RemoteEventReactionSync"
	case RemoteEventMessageRemove:
		return "RemoteEventMessageRemove"
	case RemoteEventReadReceipt:
		return "RemoteEventReadReceipt"
	case RemoteEventDeliveryReceipt:
		return "RemoteEventDeliveryReceipt"
	case RemoteEventMarkUnread:
		return "RemoteEventMarkUnread"
	case RemoteEventTyping:
		return "RemoteEventTyping"
	case RemoteEventChatInfoChange:
		return "RemoteEventChatInfoChange"
	case RemoteEventChatResync:
		return "RemoteEventChatResync"
	case RemoteEventChatDelete:
		return "RemoteEventChatDelete"
	case RemoteEventBackfill:
		return "RemoteEventBackfill"
	default:
		return fmt.Sprintf("RemoteEventType(%d)", int(ret))
	}
}

const (
	RemoteEventUnknown RemoteEventType = iota
	RemoteEventMessage
	RemoteEventMessageUpsert
	RemoteEventEdit
	RemoteEventReaction
	RemoteEventReactionRemove
	RemoteEventReactionSync
	RemoteEventMessageRemove
	RemoteEventReadReceipt
	RemoteEventDeliveryReceipt
	RemoteEventMarkUnread
	RemoteEventTyping
	RemoteEventChatInfoChange
	RemoteEventChatResync
	RemoteEventChatDelete
	RemoteEventBackfill
)

// RemoteEvent represents a single event from the remote network, such as a message or a reaction.
//
// When a [NetworkAPI] receives an event from the remote network, it should convert it into a [RemoteEvent]
// and pass it to the bridge for processing using [Bridge.QueueRemoteEvent].
type RemoteEvent interface {
	GetType() RemoteEventType
	GetPortalKey() networkid.PortalKey
	AddLogContext(c zerolog.Context) zerolog.Context
	GetSender() EventSender
}

type RemotePreHandler interface {
	RemoteEvent
	PreHandle(ctx context.Context, portal *Portal)
}

type RemoteChatInfoChange interface {
	RemoteEvent
	GetChatInfoChange(ctx context.Context) (*ChatInfoChange, error)
}

type RemoteChatResync interface {
	RemoteEvent
}

type RemoteChatResyncWithInfo interface {
	RemoteChatResync
	GetChatInfo(ctx context.Context, portal *Portal) (*ChatInfo, error)
}

type RemoteChatResyncBackfill interface {
	RemoteChatResync
	CheckNeedsBackfill(ctx context.Context, latestMessage *database.Message) (bool, error)
}

type RemoteChatDelete interface {
	RemoteEvent
	DeleteOnlyForMe() bool
}

type RemoteEventThatMayCreatePortal interface {
	RemoteEvent
	ShouldCreatePortal() bool
}

type RemoteEventWithTargetMessage interface {
	RemoteEvent
	GetTargetMessage() networkid.MessageID
}

type RemoteEventWithBundledParts interface {
	RemoteEventWithTargetMessage
	GetTargetDBMessage() []*database.Message
}

type RemoteEventWithTargetPart interface {
	RemoteEventWithTargetMessage
	GetTargetMessagePart() networkid.PartID
}

type RemoteEventWithTimestamp interface {
	RemoteEvent
	GetTimestamp() time.Time
}

type RemoteMessage interface {
	RemoteEvent
	GetID() networkid.MessageID
	ConvertMessage(ctx context.Context, portal *Portal, intent MatrixAPI) (*ConvertedMessage, error)
}

type UpsertResult struct {
	SubEvents               []RemoteEvent
	SaveParts               bool
	ContinueMessageHandling bool
}

type RemoteMessageUpsert interface {
	RemoteMessage
	HandleExisting(ctx context.Context, portal *Portal, intent MatrixAPI, existing []*database.Message) (UpsertResult, error)
}

type RemoteMessageWithTransactionID interface {
	RemoteMessage
	GetTransactionID() networkid.TransactionID
}

type RemoteEdit interface {
	RemoteEventWithTargetMessage
	ConvertEdit(ctx context.Context, portal *Portal, intent MatrixAPI, existing []*database.Message) (*ConvertedEdit, error)
}

type RemoteReaction interface {
	RemoteEventWithTargetMessage
	GetReactionEmoji() (string, networkid.EmojiID)
}

type ReactionSyncUser struct {
	Reactions []*BackfillReaction
	// Whether the list contains all reactions the user has sent
	HasAllReactions bool
	// If the list doesn't contain all reactions from the user,
	// then this field can be set to remove old reactions if there are more than a certain number.
	MaxCount int
}

type ReactionSyncData struct {
	Users map[networkid.UserID]*ReactionSyncUser
	// Whether the map contains all users who have reacted to the message
	HasAllUsers bool
}

func (rsd *ReactionSyncData) ToBackfill() []*BackfillReaction {
	var reactions []*BackfillReaction
	for _, user := range rsd.Users {
		reactions = append(reactions, user.Reactions...)
	}
	return reactions
}

type RemoteReactionSync interface {
	RemoteEventWithTargetMessage
	GetReactions() *ReactionSyncData
}

type RemoteReactionWithExtraContent interface {
	RemoteReaction
	GetReactionExtraContent() map[string]any
}

type RemoteReactionWithMeta interface {
	RemoteReaction
	GetReactionDBMetadata() any
}

type RemoteReactionRemove interface {
	RemoteEventWithTargetMessage
	GetRemovedEmojiID() networkid.EmojiID
}

type RemoteMessageRemove interface {
	RemoteEventWithTargetMessage
}

// Deprecated: Renamed to RemoteReadReceipt.
type RemoteReceipt = RemoteReadReceipt

type RemoteReadReceipt interface {
	RemoteEvent
	GetLastReceiptTarget() networkid.MessageID
	GetReceiptTargets() []networkid.MessageID
	GetReadUpTo() time.Time
}

type RemoteDeliveryReceipt interface {
	RemoteEvent
	GetReceiptTargets() []networkid.MessageID
}

type RemoteMarkUnread interface {
	RemoteEvent
	GetUnread() bool
}

type RemoteTyping interface {
	RemoteEvent
	GetTimeout() time.Duration
}

type RemoteBackfill interface {
	RemoteEvent
	GetBackfillData(ctx context.Context, portal *Portal) (*FetchMessagesResponse, error)
}

type TypingType int

const (
	TypingTypeText TypingType = iota
	TypingTypeUploadingMedia
	TypingTypeRecordingMedia
)

type RemoteTypingWithType interface {
	RemoteTyping
	GetTypingType() TypingType
}

type OrigSender struct {
	User *User
	event.MemberEventContent
}

type MatrixEventBase[ContentType any] struct {
	// The raw event being bridged.
	Event *event.Event
	// The parsed content struct of the event. Custom fields can be found in Event.Content.Raw.
	Content ContentType
	// The room where the event happened.
	Portal *Portal

	// The original sender user ID. Only present in case the event is being relayed (and Sender is not the same user).
	OrigSender *OrigSender
}

type MatrixMessage struct {
	MatrixEventBase[*event.MessageEventContent]
	ThreadRoot *database.Message
	ReplyTo    *database.Message
}

type MatrixEdit struct {
	MatrixEventBase[*event.MessageEventContent]
	EditTarget *database.Message
}

type MatrixReaction struct {
	MatrixEventBase[*event.ReactionEventContent]
	TargetMessage *database.Message
	PreHandleResp *MatrixReactionPreResponse

	// When EmojiID is blank and there's already an existing reaction, this is the old reaction that is being overridden.
	ReactionToOverride *database.Reaction
	// When MaxReactions is >0 in the pre-response, this is the list of previous reactions that should be preserved.
	ExistingReactionsToKeep []*database.Reaction
}

type MatrixReactionPreResponse struct {
	SenderID     networkid.UserID
	EmojiID      networkid.EmojiID
	Emoji        string
	MaxReactions int
}

type MatrixReactionRemove struct {
	MatrixEventBase[*event.RedactionEventContent]
	TargetReaction *database.Reaction
}

type MatrixMessageRemove struct {
	MatrixEventBase[*event.RedactionEventContent]
	TargetMessage *database.Message
}

type MatrixRoomMeta[ContentType any] struct {
	MatrixEventBase[ContentType]
	PrevContent ContentType
}

type MatrixRoomName = MatrixRoomMeta[*event.RoomNameEventContent]
type MatrixRoomAvatar = MatrixRoomMeta[*event.RoomAvatarEventContent]
type MatrixRoomTopic = MatrixRoomMeta[*event.TopicEventContent]

type MatrixReadReceipt struct {
	Portal *Portal
	// The event ID that the receipt is targeting
	EventID id.EventID
	// The exact message that was read. This may be nil if the event ID isn't a message.
	ExactMessage *database.Message
	// The timestamp that the user has read up to. This is either the timestamp of the message
	// (if one is present) or the timestamp of the receipt.
	ReadUpTo time.Time
	// The ReadUpTo timestamp of the previous message
	LastRead time.Time
	// The receipt metadata.
	Receipt event.ReadReceipt
}

type MatrixTyping struct {
	Portal   *Portal
	IsTyping bool
	Type     TypingType
}

type MatrixMarkedUnread = MatrixRoomMeta[*event.MarkedUnreadEventContent]
type MatrixMute = MatrixRoomMeta[*event.BeeperMuteEventContent]
type MatrixRoomTag = MatrixRoomMeta[*event.TagEventContent]
