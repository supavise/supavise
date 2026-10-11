package advisors

import "fmt"

// AuthSettings are the settings of a project's Auth service the lints computed outside SQL read.
// internal/api fills it from the project's saved settings and defaults.
type AuthSettings struct {
	// EmailEnabled is external_email_enabled.
	EmailEnabled bool
	// MailerOTPExp is mailer_otp_exp, in seconds.
	MailerOTPExp int64
	// DBMaxPoolSize and DBMaxPoolSizeUnit ("connections" or "percent") are db_max_pool_size and
	// db_max_pool_size_unit.
	DBMaxPoolSize     int64
	DBMaxPoolSizeUnit string
}

// OTPExpiryLimit is the longest email OTP expiry, in seconds, that does not raise
// auth_otp_long_expiry: the hour hosted's going-into-production checklist recommends.
const OTPExpiryLimit = 3600

// ConfigLints returns the lints hosted computes from a project's configuration and not from its
// schema, for the ones Supavise can: the Auth settings. The names, levels, categories and links
// are those of hosted's Management API specs. auth_db_connections_absolute is word for word what
// the recorded responses of the Supabase CLI's end-to-end suite (v2.120.0) hold for the same
// finding; auth_otp_long_expiry's texts come from hosted's published advisor output and no
// recording of it exists.
//
// A new hosted project answers auth_db_connections_absolute alone (db_max_pool_size 10 connections;
// Supavise starts at 5) and not auth_otp_long_expiry, because its mailer_otp_exp is 3600. Supavise
// starts at GoTrue's own 86400, so a new Supavise project also answers auth_otp_long_expiry until its
// OTP expiry is an hour or less. The lint reports the effective setting, as hosted's does.
//
// Not computed, and why:
//   - auth_leaked_password_protection and auth_insufficient_mfa_options: hosted projects have been
//     reported to show them, but the CLI suite's recording of a hosted project whose organization
//     is entitled to leaked password protection and every MFA method has an empty security answer
//     while password_hibp_enabled is false and no MFA method is on. The rule that makes hosted
//     answer them is not known, and a guess adds findings a default hosted project does not show
//     (a check that fails on warnings would fail every new project). They come back when a
//     recording shows what triggers them.
//   - ssl_not_enforced, log_connections_not_enabled, network_restrictions_not_set, pitr_not_enabled,
//     password_requirements_min_length, auth_password_policy_missing, no_backup_admin and
//     leaked_service_key: the specs list them in one block with the two above, and metadata.type has
//     a "compliance" kind. Published advisor output of ordinary hosted projects does not list
//     them, so they look like hosted's compliance checks. Supavise has no add-on to key them on,
//     and firing them on every project would add findings hosted does not show.
//   - auth_otp_short_length: Auth refuses an OTP shorter than 6 digits, the length hosted
//     asks for, so the finding cannot arise.
//   - auth_allow_anonymous_sign_ins: the SQL Studio redistributes does not carry it, and its
//     trigger and texts live in a repository without a license.
//   - vulnerable_postgres_version: needs a feed of security releases per Postgres version.
func ConfigLints(a AuthSettings) []Lint {
	var out []Lint
	if a.EmailEnabled && a.MailerOTPExp > OTPExpiryLimit {
		out = append(out, build(fields{
			Name:        "auth_otp_long_expiry",
			Title:       "Auth OTP long expiry",
			Level:       LevelWarn,
			Categories:  []string{CategorySecurity},
			Description: "OTP expiry exceeds recommended threshold",
			Detail:      "We have detected that you have enabled the email provider with the OTP expiry set to more than an hour. It is recommended to set this value to less than an hour.",
			CacheKey:    "auth_otp_long_expiry",
			Remediation: DocsURL + "/guides/platform/going-into-prod#security",
			Metadata:    authMetadata(),
		}))
	}
	if a.DBMaxPoolSizeUnit != "percent" {
		out = append(out, build(fields{
			Name:        "auth_db_connections_absolute",
			Title:       "Auth DB Connection Strategy is not Percentage",
			Level:       LevelInfo,
			Categories:  []string{CategoryPerformance},
			Description: "Using a percentage based allocation connection strategy for Auth can help to improve the server's performance when increasing instance size.",
			Detail:      fmt.Sprintf("Your project's Auth server is configured to use at most %d connections. Increasing the instance size without manually adjusting this number will not improve the performance of the Auth server. Switch to a percentage based connection allocation strategy instead.", a.DBMaxPoolSize),
			CacheKey:    "auth_db_connections_absolute",
			Remediation: DocsURL + "/guides/deployment/going-into-prod",
			Metadata:    authMetadata(),
		}))
	}
	return out
}

func authMetadata() map[string]any { return map[string]any{"type": "auth", "entity": "Auth"} }

// configNames are the lints ConfigLints can return.
var configNames = map[string]bool{
	"auth_otp_long_expiry":         true,
	"auth_db_connections_absolute": true,
}

// IsConfigLint reports whether name is a lint ConfigLints computes.
func IsConfigLint(name string) bool { return configNames[name] }
