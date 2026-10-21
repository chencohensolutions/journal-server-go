package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/skip2/go-qrcode"
)

// runAdminCommand implements the enroll-code, devices and revoke commands as GraphQL calls to the
// running server's admin API.
func runAdminCommand(config auth.AppConfig, command string, args []string) error {
	switch {
	case command == "enroll-code" && len(args) == 1:
		return enrollCodeCommand(config, args[0])
	case command == "devices" && len(args) <= 1:
		return devicesCommand(config, args)
	case command == "revoke" && len(args) == 1:
		return revokeCommand(config, args[0])
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command or wrong arguments: %s", command)
	}
}

type adminDevice struct {
	Serial        string     `json:"serial"`
	User          string     `json:"user"`
	Name          string     `json:"name"`
	KeyProtection string     `json:"keyProtection"`
	RegisteredAt  time.Time  `json:"registeredAt"`
	ExpiresAt     time.Time  `json:"expiresAt"`
	RevokedAt     *time.Time `json:"revokedAt"`
}

const deviceFields = "serial user name keyProtection registeredAt expiresAt revokedAt"

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
	fmt.Printf("Enrollment code for user %q\n\n", code.User)
	if code.ServerURL != nil {
		fmt.Printf("  Server   %s\n", *code.ServerURL)
	} else {
		fmt.Printf("  Server   (set public_url in config.json to show the address here)\n")
	}
	fmt.Printf("  Code     %s\n", code.Code)
	fmt.Printf("  Expires  %s (in %d min), single use\n", code.ExpiresAt.Local().Format("15:04"), int(time.Until(code.ExpiresAt).Round(time.Minute).Minutes()))
	link := enrollmentLink(code.Code, code.ServerURL)
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
		b.WriteString("\x1b[97;40m" + line + "\x1b[0m\n")
	}
	return b.String()
}

// enrollmentLink is what the enrollment QR code holds: journal://enroll?code=…&server=…, which opens the
// app's enrollment on that code. Without a server the app keeps the address it has.
func enrollmentLink(code string, serverURL *string) string {
	query := url.Values{"code": {code}}
	if serverURL != nil {
		query.Set("server", *serverURL)
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
	fmt.Fprintln(w, "USER\tDEVICE\tKEY\tREGISTERED\tEXPIRES\tSTATUS\tSERIAL")
	for _, d := range data.Devices {
		status := "active"
		if d.RevokedAt != nil {
			status = "revoked " + d.RevokedAt.Local().Format(time.DateOnly)
		} else if time.Now().After(d.ExpiresAt) {
			status = "expired"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.User, d.Name, d.KeyProtection,
			d.RegisteredAt.Local().Format(time.DateOnly), d.ExpiresAt.Local().Format(time.DateOnly), status, d.Serial)
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
