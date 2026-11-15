// Package discord is the monorepo's Discord server, as harness serve uses
// it: messages it posts and keeps up to date in named channels, threads
// under them, and buttons people press.
package discord

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Message is what the service posts: an embed, the files shown with it,
// and a button.
type Message struct {
	Title       string
	URL         string
	Description string
	Color       int
	Files       []File
	Button      *Button
}

type File struct {
	Name, ContentType string
	Data              []byte
}

// Button is pressed by someone in the server; ID says what for.
type Button struct {
	ID, Label string
	Disabled  bool
}

// Press is the answer to a button: what the person who pressed it is told,
// which only they see.
type Press func(user, id string) string

// Bot is the service's connection to one server.
type Bot struct {
	s        *discordgo.Session
	server   string
	channels map[string]string // by name
}

// Open connects to the server, as the bot whose token it is, and answers
// buttons with press.
func Open(token, server string, press Press) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuilds
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionMessageComponent || i.Member == nil {
			return
		}
		text := press(i.Member.User.ID, i.MessageComponentData().CustomID)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral},
		})
	})
	if err := s.Open(); err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	b := &Bot{s: s, server: server, channels: map[string]string{}}
	chans, err := s.GuildChannels(server)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("discord: the server's channels: %w", err)
	}
	for _, c := range chans {
		if c.Type == discordgo.ChannelTypeGuildText {
			b.channels[c.Name] = c.ID
		}
	}
	return b, nil
}

func (b *Bot) Close() { b.s.Close() }

func (b *Bot) channel(name string) (string, error) {
	if id := b.channels[name]; id != "" {
		return id, nil
	}
	return "", fmt.Errorf("discord: the server has no #%s", name)
}

func (m Message) embeds() []*discordgo.MessageEmbed {
	return []*discordgo.MessageEmbed{{Title: m.Title, URL: m.URL, Description: m.Description, Color: m.Color}}
}

func (m Message) components() []discordgo.MessageComponent {
	if m.Button == nil {
		return []discordgo.MessageComponent{}
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: m.Button.Label, CustomID: m.Button.ID, Style: discordgo.SuccessButton, Disabled: m.Button.Disabled},
	}}}
}

func (m Message) files() []*discordgo.File {
	var out []*discordgo.File
	for _, f := range m.Files {
		out = append(out, &discordgo.File{Name: f.Name, ContentType: f.ContentType, Reader: strings.NewReader(string(f.Data))})
	}
	return out
}

// Post posts a message in a channel, by name, and returns its ID.
func (b *Bot) Post(channel string, m Message) (string, error) {
	id, err := b.channel(channel)
	if err != nil {
		return "", err
	}
	msg, err := b.s.ChannelMessageSendComplex(id, &discordgo.MessageSend{
		Embeds: m.embeds(), Components: m.components(), Files: m.files(),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

// Edit replaces a message posted in a channel. Its files are replaced too,
// when files is true; otherwise they are left as they were.
func (b *Bot) Edit(channel, id string, m Message, files bool) error {
	ch, err := b.channel(channel)
	if err != nil {
		return err
	}
	embeds, components := m.embeds(), m.components()
	e := &discordgo.MessageEdit{ID: id, Channel: ch, Embeds: &embeds, Components: &components}
	if files {
		e.Attachments = &[]*discordgo.MessageAttachment{}
		e.Files = m.files()
	}
	_, err = b.s.ChannelMessageEditComplex(e)
	return err
}

// Thread starts a thread under a message posted in a channel, and returns
// its ID.
func (b *Bot) Thread(channel, message, name string) (string, error) {
	ch, err := b.channel(channel)
	if err != nil {
		return "", err
	}
	if len(name) > 100 {
		name = name[:100]
	}
	t, err := b.s.MessageThreadStart(ch, message, name, 10080)
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

// Say posts text in a thread, by its ID. Users it mentions are pinged.
func (b *Bot) Say(thread, text string) error {
	_, err := b.s.ChannelMessageSendComplex(thread, &discordgo.MessageSend{Content: text,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers}}})
	return err
}

// SayMessage posts a message, with its files and button, in a thread.
func (b *Bot) SayMessage(thread string, m Message) error {
	_, err := b.s.ChannelMessageSendComplex(thread, &discordgo.MessageSend{
		Embeds: m.embeds(), Components: m.components(), Files: m.files(),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	return err
}

// Last is when the newest message in a channel was posted, or the zero
// time if it has none.
func (b *Bot) Last(channel string) (time.Time, error) {
	ch, err := b.channel(channel)
	if err != nil {
		return time.Time{}, err
	}
	msgs, err := b.s.ChannelMessages(ch, 1, "", "", "")
	if err != nil || len(msgs) == 0 {
		return time.Time{}, err
	}
	return msgs[0].Timestamp, nil
}
