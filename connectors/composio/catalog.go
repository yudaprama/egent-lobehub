package composio

// AppType mirrors ComposioAppType in lobehub/packages/const/src/composio.ts.
// The catalog is intentionally pinned (not fetched) because LobeHub's UI and
// i18n strings key off these identifiers; allowing arbitrary app slugs would
// break the rendering layer. Add new apps here when they are added in TS.
type AppType struct {
	// AppSlug is the upper-snake slug used by the Composio REST API as the
	// toolkit_slug query parameter (e.g. "GMAIL", "GOOGLECALENDAR").
	AppSlug string `json:"appSlug"`
	// Identifier is the kebab-case slug used by LobeHub's plugin layer
	// (e.g. "gmail", "google-calendar"). It is the user-facing key stored
	// in PluginModel.identifier and matched against COMPOSIO_APP_TYPES.
	Identifier string `json:"identifier"`
	// Label is the human-readable name shown in the UI connect/disconnect
	// screen.
	Label string `json:"label"`
	// Description is the short one-line summary shown under the label.
	Description string `json:"description"`
	// Icon is the URL to the SVG logo served from the lobehub asset bucket.
	Icon string `json:"icon"`
	// Readme is the long-form description shown when the user expands an
	// app card before connecting.
	Readme string `json:"readme"`
	// Author is always "Composio" today but kept as a field for parity
	// with the TS type in case custom toolkits are added later.
	Author string `json:"author"`
	// AuthorURL is the link back to composio.dev.
	AuthorURL string `json:"authorUrl,omitempty"`
}

// COMPOSIO_APP_TYPES is the supported-app catalog. It is a port of
// lobehub/packages/const/src/composio.ts — keep the two lists in sync when
// adding or removing an app.
//
// The order here matches the TS source so a diff between the two files is
// easy to review during the bi-annual catalog refresh.
var COMPOSIO_APP_TYPES = []AppType{
	{AppSlug: "GMAIL", Identifier: "gmail", Label: "Gmail", Description: "Gmail is a free email service provided by Google", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/gmail.svg", Readme: "Bring the power of Gmail directly into your AI assistant. Read, compose, and send emails, search your inbox, manage labels, and organize your communications—all through natural conversation.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "GOOGLECALENDAR", Identifier: "google-calendar", Label: "Google Calendar", Description: "Google Calendar is a time-management and scheduling calendar service", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/googlecalendar.svg", Readme: "Integrate Google Calendar to view, create, and manage your events seamlessly. Schedule meetings, set reminders, check availability, and coordinate your time—all through natural language commands.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "AIRTABLE", Identifier: "airtable", Label: "Airtable", Description: "Airtable is a cloud-based database and spreadsheet platform that combines the flexibility of a spreadsheet with the power of a database", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/airtable.svg", Readme: "Integrate with Airtable to manage your databases and workflows. Query records, create entries, update data, and automate operations with customizable views and powerful tracking features.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "GOOGLESHEETS", Identifier: "google-sheets", Label: "Google Sheets", Description: "Google Sheets is a web-based spreadsheet application that allows users to create, edit, and collaborate on spreadsheets online", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/googlesheets.svg", Readme: "Connect to Google Sheets to read, write, and analyze spreadsheet data. Perform calculations, generate reports, create charts, and manage tabular data collaboratively with AI assistance.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "GOOGLEDOCS", Identifier: "google-docs", Label: "Google Docs", Description: "Google Docs is a word processor included as part of the free, web-based Google Docs Editors suite", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/googledocs.svg", Readme: "Integrate with Google Docs to create, edit, and manage documents. Write content, format text, collaborate in real-time, and access your documents through natural conversation.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "SUPABASE", Identifier: "supabase", Label: "Supabase", Description: "Supabase open source Firebase alternative with PostgreSQL", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/supabase.svg", Readme: "Integrate with Supabase to manage your database and backend services. Query data, manage authentication, handle storage, and interact with your application backend through natural conversation.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "GOOGLEDRIVE", Identifier: "google-drive", Label: "Google Drive", Description: "Google Drive is a cloud storage service", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/googledrive.svg", Readme: "Connect to Google Drive to access, organize, and manage your files. Search documents, upload files, share content, and navigate your cloud storage efficiently through AI assistance.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "SLACK", Identifier: "slack", Label: "Slack", Description: "Slack is a messaging app for business that connects people to the information they need", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/slack.svg", Readme: "Integrate with Slack to send messages, search conversations, and manage channels. Connect with your team, automate communication workflows, and access workspace information through natural language.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "CONFLUENCE", Identifier: "confluence", Label: "Confluence", Description: "Confluence is a team workspace where knowledge and collaboration meet", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/confluence.svg", Readme: "Connect to Confluence to access and manage team documentation. Search pages, create content, organize spaces, and build your knowledge base through conversational AI assistance.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "JIRA", Identifier: "jira", Label: "Jira", Description: "Jira is a project management and issue tracking tool developed by Atlassian", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/jira.svg", Readme: "Integrate with Jira to manage issues, track progress, and organize sprints. Create tickets, update statuses, query project data, and streamline your development workflow through natural conversation.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "CLICKUP", Identifier: "clickup", Label: "ClickUp", Description: "ClickUp is a comprehensive project management and productivity platform", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/clickup.svg", Readme: "Connect to ClickUp to manage tasks, track projects, and organize your work. Create tasks, update statuses, manage custom workflows, and collaborate with your team through natural language commands.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "DROPBOX", Identifier: "dropbox", Label: "Dropbox", Description: "Complete file management solution for Dropbox cloud storage", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/dropbox.svg", Readme: "Integrate with Dropbox to access and manage your files. Upload, download, share files, manage folders, handle file versions, and organize your cloud storage through conversational AI.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "FIGMA", Identifier: "figma", Label: "Figma", Description: "Figma is a collaborative interface design tool for web and mobile applications.", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/figma.svg", Readme: "Connect to Figma to access design files and collaborate on projects. View designs, export assets, browse components, and manage your design workflow through natural conversation.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "HUBSPOT", Identifier: "hubspot", Label: "HubSpot", Description: "HubSpot is a developer and marketer of software products for inbound marketing, sales, and customer service", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/hubspot.svg", Readme: "Integrate with HubSpot to manage contacts, deals, and marketing campaigns. Access CRM data, track pipelines, automate workflows, and streamline your sales and marketing operations.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "ONEDRIVE", Identifier: "onedrive", Label: "OneDrive", Description: "OneDrive is a file hosting service and synchronization service operated by Microsoft", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/onedrive.svg", Readme: "Connect to OneDrive to access and manage your Microsoft cloud files. Upload, download, share files, organize folders, and collaborate on documents through AI-powered assistance.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "OUTLOOK", Identifier: "outlook-mail", Label: "Outlook Mail", Description: "Outlook Mail is a web-based suite of webmail, contacts, tasks, and calendaring services from Microsoft.", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/outlook.svg", Readme: "Integrate with Outlook Mail to read, send, and manage your Microsoft emails. Search messages, compose emails, manage folders, and organize your inbox through natural conversation.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "SALESFORCE", Identifier: "salesforce", Label: "Salesforce", Description: "Salesforce is the world's leading customer relationship management (CRM) platform", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/salesforce.svg", Readme: "Connect to Salesforce to manage customer relationships and sales data. Query records, update opportunities, track leads, and automate your CRM workflows through natural language commands.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "WHATSAPP", Identifier: "whatsapp", Label: "WhatsApp", Description: "WhatsApp Business API integration that enables sending text messages, media, and managing conversations with customers.", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/whatsapp.svg", Readme: "Integrate with WhatsApp Business to send messages, manage conversations, and engage with customers. Automate messaging workflows and handle communications through conversational AI.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "YOUTUBE", Identifier: "youtube", Label: "YouTube", Description: "YouTube is a video-sharing platform where users can upload, share, and discover content. Access video information, transcripts, and metadata programmatically.", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/youtube.svg", Readme: "Connect to YouTube to search videos, access transcripts, and retrieve video information. Analyze content, extract metadata, and discover videos through natural conversation.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "ZENDESK", Identifier: "zendesk", Label: "Zendesk", Description: "Zendesk is a customer service software company", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/zendesk.svg", Readme: "Integrate with Zendesk to manage support tickets and customer interactions. Create, update, and track support requests, access customer data, and streamline your support operations.", Author: "Composio", AuthorURL: "https://composio.dev"},
	{AppSlug: "CALCOM", Identifier: "cal-com", Label: "Cal.com", Description: "Cal.com is an open-source scheduling platform that helps you schedule meetings without the back-and-forth emails.", Icon: "https://hub-apac-1.lobeobjects.space/assets/logos/calcom.svg", Readme: "Connect to Cal.com to manage your scheduling and appointments. View availability, book meetings, manage event types, and automate your calendar through natural conversation.", Author: "Composio", AuthorURL: "https://composio.dev"},
}

// GetAppByIdentifier returns the catalog entry for the given LobeHub
// identifier (e.g. "gmail", "google-calendar"). Returns nil if not found.
// Mirrors getComposioAppByIdentifier in lobehub/packages/const/src/composio.ts.
func GetAppByIdentifier(identifier string) *AppType {
	for i := range COMPOSIO_APP_TYPES {
		if COMPOSIO_APP_TYPES[i].Identifier == identifier {
			return &COMPOSIO_APP_TYPES[i]
		}
	}
	return nil
}

// GetAppBySlug returns the catalog entry whose AppSlug matches
// case-insensitively. Useful when an auth config is discovered by toolkit
// slug at runtime (the auth_configs response uses lowercase toolkit slugs).
func GetAppBySlug(slug string) *AppType {
	for i := range COMPOSIO_APP_TYPES {
		if equalFoldASCII(COMPOSIO_APP_TYPES[i].AppSlug, slug) {
			return &COMPOSIO_APP_TYPES[i]
		}
	}
	return nil
}

// IsKnownIdentifier reports whether identifier is in the supported catalog.
// LobeHub's ComposioService uses the same gate to filter manifests down to
// the supported set (lobehub/src/server/services/composio/index.ts:155-159).
func IsKnownIdentifier(identifier string) bool {
	return GetAppByIdentifier(identifier) != nil
}

// NormaliseSlug converts a LobeHub identifier ("google-calendar") or a
// user-supplied slug ("google-calendar", "GOOGLECALENDAR",
// "google_calendar") into the canonical upper-snake toolkit_slug the
// Composio REST API expects ("GOOGLECALENDAR"). Used by GetTools and
// CreateManagedAuthConfig so callers can pass either form.
func NormaliseSlug(in string) string {
	out := []byte(in)
	for i, c := range out {
		switch {
		case c >= 'a' && c <= 'z':
			out[i] = c - ('a' - 'A')
		case c == '-' || c == '_':
			out[i] = 0
		}
	}
	// Compact out the nul bytes we wrote in place of - and _.
	w := 0
	for _, c := range out {
		if c != 0 {
			out[w] = c
			w++
		}
	}
	return string(out[:w])
}

// equalFoldASCII compares two ASCII strings case-insensitively. We avoid
// strings.EqualFold because it would handle Unicode case folding we don't
// need for slug comparison, and we want zero stdlib imports in this file
// (catalog.go stays dependency-free so it can be vendored standalone).
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
