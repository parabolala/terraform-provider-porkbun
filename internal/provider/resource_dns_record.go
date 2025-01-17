package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/nrdcg/porkbun"
	"golang.org/x/exp/slices"
)

// Ensure provider defined types fully satisfy framework interfaces
var _ provider.ResourceType = porkbunDnsRecordResourceType{}
var _ resource.Resource = porkbunDnsRecordResource{}
var _ resource.ResourceWithImportState = porkbunDnsRecordResource{}

// The API returns a string of "SUCCESS" or "ERROR" except for when we're rate limited
// We get a 503 and the go library expects a string so we need to treat this as a string for now
var retryableCodes = []int{503}

var (
	sleep = 10
)

type porkbunDnsRecordResourceType struct{}

func (t porkbunDnsRecordResourceType) GetSchema(ctx context.Context) (tfsdk.Schema, diag.Diagnostics) {
	return tfsdk.Schema{
		// This description is used by the documentation generator and the language server.
		MarkdownDescription: "Example resource",

		Attributes: map[string]tfsdk.Attribute{
			"name": {
				MarkdownDescription: "The subdomain for the record itself without the base domain",
				Required:            true,
				Type:                types.StringType,
			},
			"domain": {
				Required:            true,
				MarkdownDescription: "The base domain to to create the record on",
				//PlanModifiers: tfsdk.AttributePlanModifiers{
				//	resource.UseStateForUnknown(),
				//},
				Type: types.StringType,
			},
			"id": {
				Computed:            true,
				Optional:            false,
				MarkdownDescription: "The Porkbun ID of the Record",
				Type:                types.StringType,
			},
			"ttl": {
				Optional:            true,
				MarkdownDescription: "The ttl of the record, the minimum  is 600",
				Type:                types.StringType,
			},
			"type": {
				Required:            true,
				MarkdownDescription: "The type of DNS Record to create",
				Type:                types.StringType,
			},
			"notes": {
				Optional:            true,
				MarkdownDescription: "Notes to add to the record",
				Type:                types.StringType,
			},
			"prio": {
				Optional:            true,
				MarkdownDescription: "The priority of the record",
				Type:                types.StringType,
			},
			"content": {
				Optional:            true,
				MarkdownDescription: "The content of the record",
				Type:                types.StringType,
			},
		},
	}, nil
}

func (t porkbunDnsRecordResourceType) NewResource(ctx context.Context, in provider.Provider) (resource.Resource, diag.Diagnostics) {
	provider, diags := convertProviderType(in)

	return porkbunDnsRecordResource{
		provider: provider,
	}, diags
}

type porkbunDnsRecordResourceData struct {
	Id      types.String `tfsdk:"id"`
	Name    types.String `tfsdk:"name"`
	Type    types.String `tfsdk:"type"`
	Content types.String `tfsdk:"content"`
	Ttl     types.String `tfsdk:"ttl"`
	Notes   types.String `tfsdk:"notes"`
	Prio    types.String `tfsdk:"prio"`
	Domain  types.String `tfsdk:"domain"`
}

type porkbunDnsRecordResource struct {
	provider porkbunProvider
}

func (r porkbunDnsRecordResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data porkbunDnsRecordResourceData
	attempts := r.provider.MaxRetries

	diags := req.Config.Get(ctx, &data)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	record := porkbun.Record{
		Name:    data.Name.Value,
		Type:    data.Type.Value,
		Content: data.Content.Value,
		TTL:     data.Ttl.Value,   // Minimum is 600 according to porkbun docs
		Prio:    data.Prio.Value,  // Doesn't work on .com?
		Notes:   data.Notes.Value, // Not documented
	}

	id, err := retry(attempts, sleep, func() (int, error) { return r.provider.client.CreateRecord(ctx, data.Domain.Value, record) })
	if err != nil {
		resp.Diagnostics.AddError(
			"Error creating DNS Record",
			fmt.Sprintf("Error: %s", err),
		)
	}

	data.Id = types.String{Value: fmt.Sprint(id)}

	diags = resp.State.Set(ctx, &data)
	resp.Diagnostics.Append(diags...)
}

func (r porkbunDnsRecordResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data porkbunDnsRecordResourceData
	attempts := r.provider.MaxRetries

	diags := req.State.Get(ctx, &data)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	getRecordsResult, err := retry(attempts, sleep, func() ([]porkbun.Record, error) { return r.getRecords(ctx, data.Domain.Value) })

	if err != nil {
		resp.Diagnostics.AddError(
			fmt.Sprintf(
				`Could not retrieve records for %s.`,
				data.Domain.Value,
			),
			fmt.Sprintf("Error: %s", err.Error()),
		)
	}

	tflog.Info(ctx, fmt.Sprintf("Found records: %s", getRecordsResult))
	for _, record := range getRecordsResult {
		tflog.Info(ctx, fmt.Sprintf("This record is: %s", record.ID))
		if record.ID == data.Id.Value {
			data.Content.Value = record.Content

			// This is to handle if there's no subdomain
			if data.Domain.Value == record.Name {
				data.Name.Value = ""
			} else {
				// The API returns the full record as the name so we'll strip off the domain at the end to keep it consistent
				data.Name.Value = strings.ReplaceAll(record.Name, fmt.Sprintf(".%s", data.Domain.Value), "")
			}

			data.Notes.Value = record.Notes
			data.Ttl.Value = record.TTL
			data.Type.Value = record.Type
		}
	}

	diags = resp.State.Set(ctx, &data)
	resp.Diagnostics.Append(diags...)
}

func (r porkbunDnsRecordResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data porkbunDnsRecordResourceData
	var recordId string
	attempts := r.provider.MaxRetries

	diags := req.Plan.Get(ctx, &data)
	req.State.GetAttribute(ctx, path.Root("id"), &recordId)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	record := porkbun.Record{
		Name:    data.Name.Value,
		Type:    data.Type.Value,
		Content: data.Content.Value,
		TTL:     data.Ttl.Value,   // Minimum is 600 according to porkbun docs
		Prio:    data.Prio.Value,  // Doesn't work on .com?
		Notes:   data.Notes.Value, // Not documented
	}

	intId, err := strconv.Atoi(recordId)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error converting ID to a string",
			fmt.Sprintf("Error: %s", err),
		)
	}

	err = retrySingleReturn(attempts, sleep, func() error { return r.provider.client.EditRecord(ctx, data.Domain.Value, intId, record) })
	if err != nil {
		resp.Diagnostics.AddError(
			"Error updating the record",
			fmt.Sprintf("Error %s", err),
		)
	}

	if resp.Diagnostics.HasError() {
		return
	}

	diags = resp.State.Set(ctx, &data)
	resp.State.SetAttribute(ctx, path.Root("id"), &recordId)
	resp.Diagnostics.Append(diags...)
}

func (r porkbunDnsRecordResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state porkbunDnsRecordResourceData
	attempts := r.provider.MaxRetries

	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	intId, err := strconv.Atoi(state.Id.Value)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error converting ID to a string",
			fmt.Sprintf("Error: %s", err),
		)
	}

	err = retrySingleReturn(attempts, sleep, func() error { return r.provider.client.DeleteRecord(ctx, state.Domain.Value, intId) })
	if err != nil {
		resp.Diagnostics.AddError(
			"Error deleting record",
			fmt.Sprintf("Error: %s", err),
		)
	}

	if resp.Diagnostics.HasError() {
		return
	}
}

func (r porkbunDnsRecordResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// Originally from https://stackoverflow.com/questions/67069723/keep-retrying-a-function-in-golang
func retry[T any](attempts int, sleep int, f func() (T, error)) (result T, err error) {
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(sleep) * time.Second)
			sleep *= 2
		}
		result, err = f()
		if err == nil {
			return result, nil
		}
		status, ok := err.(porkbun.Status)
		if ok {
			if status.Status != "SUCCESS" {
				return result, fmt.Errorf("cannot be retried: %s", status.Error())
			}
		}
		servererr, ok := err.(*porkbun.ServerError)
		if ok {
			if !isRetryable(servererr.StatusCode) {
				return result, fmt.Errorf("received error is not retryable: %s", servererr.Error())
			}
		}
	}
	return result, fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}

func retrySingleReturn(attempts int, sleep int, f func() error) (err error) {
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(sleep) * time.Second)
			sleep *= 2
		}
		err = f()
		if err == nil {
			return nil
		}
		err, ok := err.(*porkbun.ServerError)
		if ok {
			if !isRetryable(err.StatusCode) {
				return fmt.Errorf("received error is not retryable: %s", err)
			}
		}
	}
	return fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}

func (r porkbunDnsRecordResource) getRecords(ctx context.Context, domain string) ([]porkbun.Record, error) {
	records, err := r.provider.client.RetrieveRecords(ctx, domain)
	if err != nil {
		return []porkbun.Record{}, err
	}
	return records, nil
}

func isRetryable(status int) bool {
	return slices.Contains(retryableCodes, status)
}
