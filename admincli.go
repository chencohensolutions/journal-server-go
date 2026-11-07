package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/discovery"
	"github.com/skip2/go-qrcode"
)

// runAdminCommand implements the enroll-code, devices, revoke and private commands as GraphQL calls to the
// running server's admin API.
func runAdminCommand(config auth.AppConfig, command string, args []string) error {
	switch {
	case command == "enroll-code" && len(args) == 1:
		return enrollCodeCommand(config, args[0])
	case command == "devices" && len(args) <= 1:
		return devicesCommand(config, args)
	case command == "revoke" && len(args) == 1:
		return revokeCommand(config, args[0])
	case command == "private" && len(args) == 2 && (args[1] == "withhold" || args[1] == "allow"):
		return privateCommand(config, args[0], args[1] == "withhold")
	case command == "copy" && len(args) >= 1 && args[0] == "report":
		return copyReportCommand(config, args[1:])
	case command == "copy" && len(args) <= 1:
		return startCopyCommand(config, args)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command or wrong arguments: %s", command)
	}
}

type adminDevice struct {
	Serial          string     `json:"serial"`
	User            string     `json:"user"`
	Name            string     `json:"name"`
	KeyProtection   string     `json:"keyProtection"`
	RegisteredAt    time.Time  `json:"registeredAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	RevokedAt       *time.Time `json:"revokedAt"`
	WithholdPrivate bool       `json:"withholdPrivate"`
}

const deviceFields = "serial user name keyProtection registeredAt expiresAt revokedAt withholdPrivate"

func enrollCodeCommand(config auth.AppConfig, user string) error {
	var data struct {
		CreateEnrollmentCode struct {
			Code      string    `json:"code"`
			User      string    `json:"user"`
			ExpiresAt time.Time `json:"expiresAt"`
			ServerURL *string   `json:"serverUrl"`
		} `json:"createEnrollmentCode"`
	}
	err := adminQuery(config, `mutation($user: String!) {
		createEnrollmentCode(user: $user) { code user expiresAt serverUrl }
	}`, map[string]any{"user": user}, &data)
	if err != nil {
		return err
	}
	code := data.CreateEnrollmentCode
	// Who this server is and where, so the phone can pin its key from the first connection and find it without a
	// host name the certificate lists (docs/authentication.md, "Enrolling by id").
	var found *enrollDiscovery
	if d, err := openDiscovery(config); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		found = &enrollDiscovery{Fingerprint: d.fingerprint, Addresses: d.publisher.Addresses(ctx)}
		cancel()
		if d.key != nil {
			found.Key = hex.EncodeToString(d.key.Public().(ed25519.PublicKey))
		}
		if code.ServerURL == nil && len(found.Addresses) > 0 {
			code.ServerURL = &found.Addresses[0]
		}
	}
	fmt.Printf("Enrollment code for user %q\n\n", code.User)
	if code.ServerURL != nil {
		fmt.Printf("  Server   %s\n", *code.ServerURL)
	} else {
		fmt.Printf("  Server   (set public_url in config.json to show the address here)\n")
	}
	if found != nil {
		fmt.Printf("  Id       %s\n", discovery.FormatID(found.Fingerprint))
	}
	fmt.Printf("  Code     %s\n", code.Code)
	fmt.Printf("  Expires  %s (in %d min), single use\n", code.ExpiresAt.Local().Format("15:04"), int(time.Until(code.ExpiresAt).Round(time.Minute).Minutes()))
	link := enrollmentLink(code.Code, code.ServerURL, found)
	qr, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Printf("\nScan with the phone's camera to open the app with this code:\n\n%s\n  %s\n", terminalQR(qr), link)
	return nil
}

// terminalQR draws qr two rows to a line, white on black whatever the terminal's colors, so it is never inverted
// (some cameras don't read an inverted code).
func terminalQR(qr *qrcode.QRCode) string {
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(qr.ToSmallString(false), "\n"), "\n") {
		b.WriteString("\x1b[97;40m")
		b.WriteString(line)
		b.WriteString("\x1b[0m\n")
	}
	return b.String()
}

// enrollDiscovery is who the server is and where, for the enrollment link.
type enrollDiscovery struct {
	Fingerprint string   // peers.Fingerprint of the server certificate
	Key         string   // the DHT key, hex; "" with discovery off
	Addresses   []string // https://host:port, the most likely first
}

// enrollmentLink is what the enrollment QR code holds: journal://enroll?code=…&server=…, which opens the
// app's enrollment on that code. Without a server the app keeps the address it has. With found it also carries
// fingerprint=… (the app checks the server's key against it instead of the host name, from the first
// connection), key=… (to look the server up in the DHT when no address answers) and address=… for each of the
// server's addresses.
func enrollmentLink(code string, serverURL *string, found *enrollDiscovery) string {
	query := url.Values{"code": {code}}
	if serverURL != nil {
		query.Set("server", *serverURL)
	}
	if found != nil {
		query.Set("fingerprint", found.Fingerprint)
		if found.Key != "" {
			query.Set("key", found.Key)
		}
		for _, a := range found.Addresses[:min(len(found.Addresses), 6)] {
			query.Add("address", a)
		}
	}
	return "journal://enroll?" + query.Encode()
}

func devicesCommand(config auth.AppConfig, args []string) error {
	variables := map[string]any{}
	if len(args) == 1 {
		variables["user"] = args[0]
	}
	var data struct {
		Devices []adminDevice `json:"devices"`
	}
	if err := adminQuery(config, `query($user: String) { devices(user: $user) { `+deviceFields+` } }`, variables, &data); err != nil {
		return err
	}
	if len(data.Devices) == 0 {
		fmt.Println("No enrolled devices.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "USER\tDEVICE\tKEY\tREGISTERED\tEXPIRES\tSTATUS\tPRIVATE\tSERIAL")
	for _, d := range data.Devices {
		status := "active"
		if d.RevokedAt != nil {
			status = "revoked " + d.RevokedAt.Local().Format(time.DateOnly)
		} else if time.Now().After(d.ExpiresAt) {
			status = "expired"
		}
		private := "sent"
		if d.WithholdPrivate {
			private = "withheld"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.User, d.Name, d.KeyProtection,
			d.RegisteredAt.Local().Format(time.DateOnly), d.ExpiresAt.Local().Format(time.DateOnly), status, private, d.Serial)
	}
	return w.Flush()
}

func revokeCommand(config auth.AppConfig, serial string) error {
	var data struct {
		RevokeDevice adminDevice `json:"revokeDevice"`
	}
	err := adminQuery(config, `mutation($serial: String!) { revokeDevice(serial: $serial) { `+deviceFields+` } }`,
		map[string]any{"serial": serial}, &data)
	if err != nil {
		return err
	}
	fmt.Printf("Revoked %q of user %s (serial %s)\n", data.RevokeDevice.Name, data.RevokeDevice.User, data.RevokeDevice.Serial)
	return nil
}

func privateCommand(config auth.AppConfig, serial string, withhold bool) error {
	var data struct {
		SetWithholdPrivate adminDevice `json:"setWithholdPrivate"`
	}
	err := adminQuery(config, `mutation($serial: String!, $withhold: Boolean!) {
		setWithholdPrivate(serial: $serial, withhold: $withhold) { `+deviceFields+` }
	}`, map[string]any{"serial": serial, "withhold": withhold}, &data)
	if err != nil {
		return err
	}
	d := data.SetWithholdPrivate
	if d.WithholdPrivate {
		fmt.Printf("Private files are kept from %q of user %s; its next sync takes back those it has\n", d.Name, d.User)
	} else {
		fmt.Printf("Private files are sent to %q of user %s again (it hides them unless Show private media is on)\n", d.Name, d.User)
	}
	return nil
}

type adminCopy struct {
	User     string `json:"user"`
	Folder   string `json:"folder"`
	At       string `json:"at"`
	Realtime bool   `json:"realtime"`
	SameDisk bool   `json:"sameDisk"`
	Running  *struct {
		StartedAt time.Time `json:"startedAt"`
		Files     int       `json:"files"`
		Bytes     float64   `json:"bytes"`
	} `json:"running"`
	LastPass *struct {
		StartedAt  time.Time `json:"startedAt"`
		FinishedAt time.Time `json:"finishedAt"`
		Files      int       `json:"files"`
		Bytes      float64   `json:"bytes"`
		Failed     int       `json:"failed"`
		Error      *string   `json:"error"`
	} `json:"lastPass"`
	Error         *string  `json:"error"`
	Originals     int      `json:"originals"`
	OriginalBytes float64  `json:"originalBytes"`
	Sidecars      int      `json:"sidecars"`
	InCopyOnly    int      `json:"inCopyOnly"`
	Paths         []string `json:"paths"`
}

const copyFields = `user folder at realtime sameDisk running { startedAt files bytes }
	lastPass { startedAt finishedAt files bytes failed error } error originals originalBytes sidecars inCopyOnly`

// startCopyCommand is `journal-server copy [user]`: the running server starts a full pass of the copies now.
func startCopyCommand(config auth.AppConfig, args []string) error {
	variables := map[string]any{}
	if len(args) == 1 {
		variables["user"] = args[0]
	}
	var data struct {
		StartCopy []adminCopy `json:"startCopy"`
	}
	if err := adminQuery(config, `mutation($user: String) { startCopy(user: $user) { `+copyFields+` } }`, variables, &data); err != nil {
		return err
	}
	for _, c := range data.StartCopy {
		if c.Running != nil {
			fmt.Printf("%s → %s: a pass is already running\n", c.User, c.Folder)
		} else {
			fmt.Printf("%s → %s: started\n", c.User, c.Folder)
		}
	}
	fmt.Println("The server's log tells when each pass ends; journal-server copy report shows how far it is.")
	return nil
}

// copyReportCommand is `journal-server copy report [user] [--list]`: how each copy is doing and what it lacks.
func copyReportCommand(config auth.AppConfig, args []string) error {
	variables := map[string]any{"paths": false}
	for _, arg := range args {
		switch {
		case arg == "--list":
			variables["paths"] = true
		case !strings.HasPrefix(arg, "-") && variables["user"] == nil:
			variables["user"] = arg
		default:
			return fmt.Errorf("usage: journal-server copy report [user] [--list]")
		}
	}
	var data struct {
		CopyReport []adminCopy `json:"copyReport"`
	}
	err := adminQuery(config, `query($user: String, $paths: Boolean) { copyReport(user: $user, paths: $paths) { `+copyFields+` paths } }`,
		variables, &data)
	if err != nil {
		return err
	}
	if len(data.CopyReport) == 0 {
		fmt.Println("No copies in config.json.")
		return nil
	}
	for i, c := range data.CopyReport {
		if i > 0 {
			fmt.Println()
		}
		printCopy(c)
	}
	return nil
}

func printCopy(c adminCopy) {
	mode := "daily " + c.At
	if c.Realtime {
		mode = "realtime, " + mode
	}
	fmt.Printf("%s → %s   (%s)\n", c.User, c.Folder, mode)
	if c.SameDisk {
		fmt.Printf("  warning:      on the data folder's disk; a failing disk takes both\n")
	}
	if r := c.Running; r != nil {
		fmt.Printf("  running:      since %s, %d files, %s copied\n", r.StartedAt.Local().Format("Jan 2 15:04"), r.Files,
			copies.Size(int64(r.Bytes)))
	}
	switch p := c.LastPass; {
	case p == nil:
		fmt.Printf("  last pass:    none since the server started\n")
	case p.Error != nil:
		fmt.Printf("  last pass:    %s, stopped: %s (%d files copied)\n", p.StartedAt.Local().Format("Jan 2 15:04"), *p.Error, p.Files)
	default:
		fmt.Printf("  last pass:    %s, done in %s, %d files (%s), %d failed\n", p.StartedAt.Local().Format("Jan 2 15:04"),
			p.FinishedAt.Sub(p.StartedAt).Round(time.Second), p.Files, copies.Size(int64(p.Bytes)), p.Failed)
	}
	if c.Error != nil {
		fmt.Printf("  not copied:   unknown: %s\n", *c.Error)
		return
	}
	if c.Originals == 0 && c.Sidecars == 0 {
		fmt.Printf("  not copied:   nothing; the copy is up to date\n")
	} else {
		fmt.Printf("  not copied:   %d originals (%s), %d sidecars\n", c.Originals, copies.Size(int64(c.OriginalBytes)), c.Sidecars)
	}
	if c.InCopyOnly > 0 {
		fmt.Printf("  in copy only: %d originals (kept; gone from the data folder)\n", c.InCopyOnly)
	}
	for _, path := range c.Paths {
		fmt.Printf("    %s\n", path)
	}
}

func adminQuery(config auth.AppConfig, query string, variables map[string]any, out any) error {
	token, err := readAdminToken(config.AdminTokenFile)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+config.AdminAddr+"/admin/query", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the admin API at %s (is the server running?): %w", config.AdminAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin API returned %s", resp.Status)
	}

	var result struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("%s", result.Errors[0].Message)
	}
	return json.Unmarshal(result.Data, out)
}
