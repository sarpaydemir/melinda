package plane

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/prompt"
)

// fields are the work-item fields that creating and updating a work item take.
const fields = `"name":{"type":"string"},"description":{"type":"string"},"state_id":{"type":"string"},` +
	`"assignee_ids":{"type":"array","items":{"type":"string"}},"label_ids":{"type":"array","items":{"type":"string"}},` +
	`"cycle_id":{"type":"string"},"priority":{"type":"string","enum":["urgent","high","medium","low","none"]},` +
	`"start_date":{"type":"string"},"target_date":{"type":"string"},"parent_id":{"type":"string"}`

// Tools are the tools Plane contributes, in the order they are offered.
func Tools() []tool.Tool {
	return []tool.Tool{
		declared{"plane_list_projects", `{"type":"object","properties":{}}`},
		declared{"plane_get_project_metadata", `{"type":"object","properties":{"project_id":{"type":"string"}},"required":["project_id"]}`},
		declared{"plane_list_issues", `{"type":"object","properties":{"project_id":{"type":"string"},"query":{"type":"string"},` +
			`"state_id":{"type":"string"},"assignee_id":{"type":"string"},"cycle_id":{"type":"string"},"label_id":{"type":"string"},` +
			`"order_by":{"type":"string"},"limit":{"type":"integer"}},"required":["project_id"]}`},
		declared{"plane_create_issue", `{"type":"object","properties":{"project_id":{"type":"string"},` + fields +
			`},"required":["project_id","name"]}`},
		declared{"plane_update_issue", `{"type":"object","properties":{"project_id":{"type":"string"},"work_item_id":{"type":"string"},` +
			fields + `},"required":["project_id","work_item_id"]}`},
		declared{"plane_list_comments", `{"type":"object","properties":{"project_id":{"type":"string"},"work_item_id":{"type":"string"},` +
			`"limit":{"type":"integer"}},"required":["project_id","work_item_id"]}`},
		declared{"plane_list_cycle_issues", `{"type":"object","properties":{"project_id":{"type":"string"},"cycle_id":{"type":"string"},` +
			`"limit":{"type":"integer"}},"required":["project_id","cycle_id"]}`},
		declared{"plane_create_comment", `{"type":"object","properties":{"project_id":{"type":"string"},"work_item_id":{"type":"string"},` +
			`"body":{"type":"string"}},"required":["project_id","work_item_id","body"]}`},
		declared{"plane_get_work_item", `{"type":"object","properties":{"project_id":{"type":"string"},"work_item_id":{"type":"string"}},` +
			`"required":["project_id","work_item_id"]}`},
	}
}

// Prompts are the catalog entries of Plane's tools, with their default texts.
func Prompts() []prompt.Entry {
	return []prompt.Entry{
		{ID: "tools.desc.plane_list_projects", Kind: prompt.KindRaw, Default: "List the work-tracking projects, " +
			"with their IDs and identifiers."},
		{ID: "tools.desc.plane_get_project_metadata", Kind: prompt.KindRaw, Default: "Get the states, labels, " +
			"cycles, and members of a project, with the IDs the other work-item tools take."},
		{ID: "tools.desc.plane_list_issues", Kind: prompt.KindRaw, Default: "List the work items of a project, " +
			"optionally matching a text query or filtered by state, assignee, cycle, or label ID. order_by names a " +
			"field, prefixed with - for descending order; limit is at most 50."},
		{ID: "tools.desc.plane_create_issue", Kind: prompt.KindRaw, Default: "Create a work item in a project. " +
			"Give the state, assignees, labels, cycle, and parent as IDs; priority is urgent, high, medium, low, or " +
			"none; dates are YYYY-MM-DD."},
		{ID: "tools.desc.plane_update_issue", Kind: prompt.KindRaw, Default: "Change a work item. Send only the " +
			"fields to change, with IDs as for creating one."},
		{ID: "tools.desc.plane_list_comments", Kind: prompt.KindRaw, Default: "List the comments on a work item; " +
			"limit is at most 50."},
		{ID: "tools.desc.plane_list_cycle_issues", Kind: prompt.KindRaw, Default: "List the work items in a cycle " +
			"with their state, assignees, priority, and dates; limit is at most 50."},
		{ID: "tools.desc.plane_create_comment", Kind: prompt.KindRaw, Default: "Add a comment to a work item."},
		{ID: "tools.desc.plane_get_work_item", Kind: prompt.KindRaw, Default: "Show a work item in detail: state, " +
			"assignees, priority, and description."},
	}
}

// declared is a tool offered by its name and argument schema; calling it is not supported yet.
type declared struct{ name, parameters string }

func (d declared) Name() string { return d.name }

func (d declared) Parameters() json.RawMessage { return json.RawMessage(d.parameters) }

func (declared) Call(context.Context, tool.Call) (string, error) { return "", errors.ErrUnsupported }
