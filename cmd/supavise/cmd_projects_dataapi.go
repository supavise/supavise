package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/lifecycle"
)

var (
	pExposeNewTables bool
	pAPISchema       bool
	pExposedSchemas  string
)

// addDataAPIFlags adds the Data API flags of `projects create`: the fields of the create body
// (data_api_revoke_default_privileges, data_api_use_api_schema, data_api_exposed_schemas).
func addDataAPIFlags(c *cobra.Command) {
	c.Flags().BoolVar(&pExposeNewTables, "expose-new-tables", false,
		"keep the default grants on new tables and sequences of public for anon, authenticated and service_role (default: revoked, as on hosted Supabase)")
	c.Flags().BoolVar(&pAPISchema, "api-schema", false, "create a schema \"api\" and expose it through the Data API")
	c.Flags().StringVar(&pExposedSchemas, "exposed-schemas", "",
		"comma-separated schemas the Data API serves (default public,graphql_public; an empty value exposes only graphql_public, which holds no table)")
}

// dataAPIFromFlags reads the flags of addDataAPIFlags into the options of a create.
func dataAPIFromFlags(cmd *cobra.Command) (lifecycle.DataAPIOptions, error) {
	o := lifecycle.DataAPIOptions{ExposeNewTables: pExposeNewTables, APISchema: pAPISchema}
	var list []string
	if cmd.Flags().Changed("exposed-schemas") {
		list = strings.Split(pExposedSchemas, ",")
	}
	schemas, err := lifecycle.ParseExposedSchemas(list, cmd.Flags().Changed("exposed-schemas"), pAPISchema)
	if err != nil {
		return o, err
	}
	o.ExposedSchemas = schemas
	return o, nil
}
