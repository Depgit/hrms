// Command hrms runs the Saltstayz HRMS API.
//
//	hrms migrate                                  create the MongoDB collections and indexes (and reference data)
//	hrms seed [-sample]                           load reference data (and the sample organisation)
//	hrms create-admin -name … -mobile …           make the first HR Admin of a new installation (once; see below)
//	hrms serve                                    run the API and background jobs
//	hrms import-pg [-from postgres://…] [-replace [-reset-mdm]] copy an existing PostgreSQL installation into MongoDB
//	                                              (-replace keeps the CMS link; -reset-mdm clears it too)
//
// create-admin [-email …] [-id EMP-0001] [-state Haryana] [-city Gurugram]: run after `hrms seed` (without -sample).
// The password is optional and is read from HRMS_ADMIN_PASSWORD, never from a flag (shell history); without it the
// person activates the account with an OTP sent to the mobile number.
//
// MONGO_URL (default mongodb://localhost:27017/?replicaSet=rs0&directConnection=true) and MONGO_DB (default hrms)
// say which database. MongoDB must run as a replica set (one node is enough): the app uses transactions.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/mail"
	"github.com/saltstayz/hrms/internal/pgimport"
	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/seed"
	"github.com/saltstayz/hrms/internal/sms"
	"github.com/saltstayz/hrms/internal/storage"
	"github.com/saltstayz/hrms/internal/store"
)

// loadDotEnv reads KEY=VALUE lines from .env without overriding the real environment.
func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && os.Getenv(strings.TrimSpace(k)) == "" {
			os.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
}

// Where the database is, unless MONGO_URL / MONGO_DB say otherwise.
const (
	defaultMongoURL = "mongodb://localhost:27017/?replicaSet=rs0&directConnection=true"
	defaultMongoDB  = "hrms"
	defaultPgURL    = "postgres://hrms:hrms@localhost:5433/hrms"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	loadDotEnv()
	// Dates ("today", the open month, leave days) are business dates in the company's zone, whatever the host's zone.
	if loc, err := time.LoadLocation(store.Zone()); err == nil {
		time.Local = loc
	} else {
		log.Printf("APP_TZ %q not loaded (%v); using the host's time zone", store.Zone(), err)
	}
	if len(os.Args) < 2 {
		fmt.Println("usage: hrms migrate | seed [-sample] | create-admin -name … -mobile … | serve | import-pg [-from postgres://…] [-replace [-reset-mdm]] | realtime-pull [-from yyyy-mm-dd] [-to yyyy-mm-dd] | realtime-match | realtime-forms | pay-compare -month yyyy-mm[,yyyy-mm]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Args[1] == "realtime-forms" {
		// Read only: where the Realtime portal's employee form is and what it holds (internal/realtime Discover), to
		// confirm realtime.Spec before the first "Send to portal". Signs in and opens pages (GET); presses nothing.
		cfg, ok := realtime.FromEnv(os.Getenv)
		if !ok {
			log.Fatal("REALTIME_URL, REALTIME_USER and REALTIME_PASS are not set")
		}
		c, err := realtime.New(cfg)
		if err == nil {
			err = c.Discover(ctx, os.Stdout)
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	st, err := store.Open(ctx, env("MONGO_URL", defaultMongoURL), env("MONGO_DB", defaultMongoDB))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	switch os.Args[1] {
	case "migrate":
		if err := st.Migrate(ctx, func(s string) { log.Println(s) }); err != nil {
			log.Fatal(err)
		}
		log.Println("migrations up to date")
	case "seed":
		fs := flag.NewFlagSet("seed", flag.ExitOnError)
		sample := fs.Bool("sample", false, "also load the sample organisation")
		_ = fs.Parse(os.Args[2:])
		if err := seed.Reference(ctx, st); err != nil {
			log.Fatal(err)
		}
		log.Println("reference data loaded")
		if *sample {
			users, err := seed.SampleOrg(ctx, st)
			if err != nil {
				log.Fatal(err)
			}
			if users == nil {
				log.Println("sample organisation already present, skipped")
			}
			for role, id := range users {
				log.Printf("  %-16s %s", role, id)
			}
			if err := seed.LinkCalendars(ctx, st); err != nil {
				log.Fatal(err)
			}
			if n, err := seed.SampleAttendance(ctx, st); err != nil {
				log.Fatal(err)
			} else if n > 0 {
				log.Printf("sample attendance: %d days of punches for the last six weeks", n)
			}
			set, err := seed.StartingLogins(ctx, st)
			if err != nil {
				log.Fatal(err)
			}
			for _, id := range set {
				log.Printf("starting login %s, password %s", id, seed.StartingPassword)
			}
			if len(set) == 0 {
				log.Printf("starting logins %s and %s already have a password, left unchanged", seed.StartingAdmin, seed.StartingUser)
			}
		}
	case "serve":
		if err := st.Migrate(ctx, func(s string) { log.Println(s) }); err != nil {
			log.Fatal(err)
		}
		if n, err := st.HealVarTargets(ctx); err != nil { // variable pay as a target on every structure (schema_vartarget.go)
			log.Fatal(err)
		} else if n > 0 {
			log.Printf("%d salary structure(s) moved to the variable pay target", n)
		}
		files := env("STORAGE_DIR", "data/uploads")
		fileStore, err := storage.FromEnv(os.Getenv)
		if err != nil {
			log.Fatal(err)
		}
		if os.Getenv("STORAGE_DRIVER") == "gcs" && os.Getenv("APP_SECRET") == "" {
			// Without APP_SECRET the key lives in a file next to STORAGE_DIR, which a container loses on restart.
			log.Fatal("STORAGE_DRIVER=gcs needs APP_SECRET (the key that encrypts the stored Google Drive sign-in)")
		}
		// The key that encrypts the Google Drive sign-in: APP_SECRET, else a key file next to STORAGE_DIR (0600).
		key, keyFrom, err := api.SecretKey(filepath.Join(filepath.Dir(filepath.Clean(files)), "hrms-secret.key"))
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("stored secrets are encrypted with the key from %s", keyFrom)
		devOTP := env("OTP_DEV_ECHO", "false") == "true"
		if devOTP && env("APP_ENV", "") == "production" {
			log.Fatal("OTP_DEV_ECHO must be false when APP_ENV=production: it would show every OTP on screen")
		}
		// The one-time code goes out by SMS through MSG91 (MSG91_AUTH_KEY + MSG91_SMS_FLOW_ID).
		var otpSend api.OTPSender
		if m := sms.FromEnv(os.Getenv); m != nil {
			otpSend = m.SendOTP
			log.Println("OTP codes are sent by SMS through MSG91")
		} else if devOTP {
			log.Println("SMS is not set up (MSG91_AUTH_KEY, MSG91_SMS_FLOW_ID): OTP_DEV_ECHO is on, so codes are shown on screen")
		} else {
			log.Println("WARNING: SMS is not set up (MSG91_AUTH_KEY, MSG91_SMS_FLOW_ID) and OTP_DEV_ECHO is off: nobody can get an OTP")
		}
		// The company mailbox for the joiner's link and code: SMTP_* settings; without them, the development echo
		// (written to this log, never sent) when OTP_DEV_ECHO is on, else none (the app says email is not set up).
		var sender mail.Sender
		if smtp, err := mail.FromEnv(os.Getenv); err != nil {
			log.Fatal(err)
		} else if smtp != nil {
			sender = smtp
			log.Printf("email is sent through %s:%d as %s", smtp.Host, smtp.Port, smtp.From)
		} else if devOTP {
			sender = mail.Echo{}
			log.Println("email is not set up: OTP_DEV_ECHO is on, so emails are written to this log instead of being sent")
		} else {
			log.Println("email is not set up (SMTP_HOST): joiner links are shared by Copy link or WhatsApp")
		}
		// The proxies in front of the API (the web server), whose X-Forwarded-For names the caller for the joiner
		// pages' rate limits; default the machine itself.
		proxies, err := api.ParseTrustedProxies(os.Getenv("TRUSTED_PROXIES"))
		if err != nil {
			log.Fatal(err)
		}
		srv := api.New(st, api.Options{DevOTP: devOTP, Files: fileStore, OTP: otpSend, SecretKey: key, Mail: sender,
			AppURL: env("APP_URL", "http://localhost:5173"), TrustedProxies: proxies})
		// Not cancelled by the signal: StopJobs below shuts the queue down gracefully.
		if err := srv.StartJobs(context.WithoutCancel(ctx)); err != nil {
			log.Fatal(err)
		}
		addr := ":" + env("PORT", "8080")
		hs := &http.Server{Addr: addr, Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = hs.Shutdown(sctx)
			srv.StopJobs(sctx) // before the database client closes
		}()
		log.Printf("HRMS API listening on %s", addr)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
		<-stopped
	case "create-admin":
		// The first HR Admin of a new installation. Needs `hrms seed` (without -sample) first; refuses once any HR Admin
		// exists. The optional password comes from HRMS_ADMIN_PASSWORD, never a flag.
		fs := flag.NewFlagSet("create-admin", flag.ExitOnError)
		in := seed.FirstAdminInput{Password: os.Getenv("HRMS_ADMIN_PASSWORD")}
		fs.StringVar(&in.Name, "name", "", "the person's full name (required)")
		fs.StringVar(&in.Mobile, "mobile", "", "10-digit mobile number; the OTP is sent here (required)")
		fs.StringVar(&in.Email, "email", "", "email address (optional)")
		fs.StringVar(&in.ID, "id", "", "Employee ID (default: the next in the series, EMP-0001)")
		fs.StringVar(&in.State, "state", "", "the head office's state (default Haryana)")
		fs.StringVar(&in.City, "city", "", "the head office's city (default Gurugram)")
		_ = fs.Parse(os.Args[2:])
		id, err := seed.FirstAdmin(ctx, st, in)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("HR Admin %s created for %s. Head office, department and shift were made with placeholder names: rename them in Masters.", id, in.Name)
		if in.Password == "" {
			log.Printf("No password was set: %s signs in with Activate with OTP (needs SMS) .", id)
		}
	case "realtime-pull":
		// Punches from the Realtime India portal for a date range (default today), as the setup page's "Pull now";
		// the account comes from REALTIME_URL, REALTIME_USER and REALTIME_PASS. The database must be migrated.
		fs := flag.NewFlagSet("realtime-pull", flag.ExitOnError)
		day := time.Now().Format("2006-01-02")
		from := fs.String("from", day, "first day (yyyy-mm-dd)")
		to := fs.String("to", "", "last day (yyyy-mm-dd; default the first day)")
		_ = fs.Parse(os.Args[2:])
		if *to == "" {
			*to = *from
		}
		srv := api.New(st, api.Options{Files: storage.Local{Root: env("STORAGE_DIR", "data/uploads")}})
		note, ok, err := srv.PullRealtime(ctx, *from, *to)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Realtime portal %s to %s: %s", *from, *to, note)
		if !ok {
			os.Exit(1)
		}
	case "realtime-match":
		// Match portal codes, as the setup page's "Fetch employee list": downloads the Realtime portal's employee
		// list and stores the proposals for review (nothing is applied). The database must be migrated.
		srv := api.New(st, api.Options{Files: storage.Local{Root: env("STORAGE_DIR", "data/uploads")}})
		note, err := srv.RealtimeMatchRun(ctx, "Command line")
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Realtime portal employee list: %s", note)
	case "pay-compare":
		// Pay components (workforce plan, module 3): each month's pay calculated as before components and as now, for
		// everyone paid in it; lists every difference (there must be none for the converted structures). Read only.
		fs := flag.NewFlagSet("pay-compare", flag.ExitOnError)
		month := fs.String("month", time.Now().Format("2006-01"), "the payroll month(s), yyyy-mm, comma separated")
		asJSON := fs.Bool("json", false, "print the full result as JSON")
		dump := fs.String("dump", "", "write every row as calculated now to this file (before a data change), and stop")
		against := fs.String("against", "", "compare the rows now with a file written by -dump (after a data change)")
		_ = fs.Parse(os.Args[2:])
		srv := api.New(st, api.Options{Files: storage.Local{Root: env("STORAGE_DIR", "data/uploads")}})
		if *dump != "" {
			all := map[string]any{}
			for _, ym := range strings.Split(*month, ",") {
				rows, err := srv.PayRows(ctx, strings.TrimSpace(ym))
				if err != nil {
					log.Fatal(err)
				}
				all[strings.TrimSpace(ym)] = rows
			}
			b, _ := json.Marshal(all)
			if err := os.WriteFile(*dump, b, 0o600); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("wrote %s\n", *dump)
			return
		}
		var saved map[string]map[string]json.RawMessage
		if *against != "" {
			b, err := os.ReadFile(*against)
			if err != nil || json.Unmarshal(b, &saved) != nil {
				log.Fatalf("cannot read %s", *against)
			}
		}
		bad := 0
		for _, ym := range strings.Split(*month, ",") {
			ym = strings.TrimSpace(ym)
			var res *api.PayCompareResult
			var err error
			if saved != nil {
				res, err = srv.PayAgainst(ctx, ym, saved[ym])
			} else {
				res, err = srv.PayCompare(ctx, ym)
			}
			if err != nil {
				log.Fatal(err)
			}
			if *asJSON {
				b, _ := json.MarshalIndent(res, "", "  ")
				fmt.Println(string(b))
			}
			fmt.Printf("%s: %d paid, %d compared (converted structures), %d on structures made from components (not compared), total net %.2f, %d with differences\n",
				res.Ym, res.People, res.Compared, res.Designed, res.Net, len(res.Diffs))
			for _, d := range res.Diffs {
				fmt.Printf("  %s %s\n", d.EmployeeID, d.Name)
				for _, f := range d.Fields {
					fmt.Printf("    %s\n", f)
				}
			}
			bad += len(res.Diffs)
		}
		if bad > 0 {
			os.Exit(1)
		}
	case "import-pg":
		fs := flag.NewFlagSet("import-pg", flag.ExitOnError)
		from := fs.String("from", env("DATABASE_URL", defaultPgURL), "the PostgreSQL database to copy (it is first brought up to date with the old SQL migrations)")
		replace := fs.Bool("replace", false, "clear the MongoDB database first if it already holds data (the CMS link, mdm_*, is kept)")
		resetMDM := fs.Bool("reset-mdm", false, "with -replace, also clear the CMS link: subscribers, feed keys, settings, outbox, published snapshots")
		_ = fs.Parse(os.Args[2:])
		log.Printf("importing %s into MongoDB database %s", redact(*from), env("MONGO_DB", defaultMongoDB))
		counts, err := pgimport.Run(ctx, st, pgimport.Options{From: *from, Replace: *replace, ResetMDM: *resetMDM, Log: func(s string) { log.Println(s) }})
		if len(counts) > 0 {
			fmt.Printf("%-28s %10s %10s\n", "collection", "postgres", "mongodb")
			for _, c := range counts {
				mark := ""
				if c.Mismatched {
					mark = "  MISMATCH"
				}
				fmt.Printf("%-28s %10d %10d%s\n", c.Table, c.Source, c.Target, mark)
			}
		}
		if err != nil {
			log.Fatal(err)
		}
		log.Println("import complete: every collection has the source's row count")
	default:
		fmt.Println("unknown command:", os.Args[1])
		os.Exit(2)
	}
}

// redact hides the password of a database URL in logs.
func redact(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		if j := strings.Index(u[i+3:], "@"); j >= 0 {
			cred := u[i+3 : i+3+j]
			if k := strings.Index(cred, ":"); k >= 0 {
				return u[:i+3] + cred[:k] + ":***" + u[i+3+j:]
			}
		}
	}
	return u
}
