package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/lifecycle"
)

func TestProjectsCreateDataAPIFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want lifecycle.DataAPIOptions
		err  bool
	}{
		{args: nil, want: lifecycle.DataAPIOptions{}},
		{args: []string{"--expose-new-tables"}, want: lifecycle.DataAPIOptions{ExposeNewTables: true}},
		{args: []string{"--api-schema"}, want: lifecycle.DataAPIOptions{APISchema: true, ExposedSchemas: "api,graphql_public"}},
		{args: []string{"--exposed-schemas", "public, app"}, want: lifecycle.DataAPIOptions{ExposedSchemas: "public,app"}},
		{args: []string{"--exposed-schemas", ""}, want: lifecycle.DataAPIOptions{ExposedSchemas: "graphql_public"}},
		{args: []string{"--exposed-schemas", `a"b`}, err: true},
	} {
		pExposeNewTables, pAPISchema, pExposedSchemas = false, false, ""
		c := &cobra.Command{Use: "create"}
		addDataAPIFlags(c)
		if err := c.ParseFlags(tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		got, err := dataAPIFromFlags(c)
		if tc.err {
			if err == nil {
				t.Errorf("%v: no error", tc.args)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%v: got %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
}
