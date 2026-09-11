package accessibility

import (
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

func webClientInteractionSmoke(page *browserPage, pageURL string, rpc *fakeRPC) error {
	if err := openPage(page, pageURL); err != nil {
		return err
	}
	var liveRegions int
	if err := page.run(chromedp.Evaluate(`document.querySelectorAll("#log[aria-live='polite']").length`, &liveRegions)); err != nil {
		return fmt.Errorf("%s: inspect task log live region: %w", pageURL, err)
	}
	if liveRegions != 1 {
		return fmt.Errorf("%s: task log must announce updates", pageURL)
	}
	if err := page.run(chromedp.Evaluate("("+injectWorkingTaskJS+")()", nil)); err != nil {
		return fmt.Errorf("%s: inject working task: %w", pageURL, err)
	}
	var cancelEnabled bool
	if err := page.run(chromedp.Evaluate(
		`!document.querySelector("button[aria-label='Cancel the active task']")?.disabled`,
		&cancelEnabled,
	)); err != nil {
		return fmt.Errorf("%s: inspect cancellation control: %w", pageURL, err)
	}
	if !cancelEnabled {
		return fmt.Errorf("%s: active task must enable cancellation", pageURL)
	}
	if err := page.run(
		chromedp.Evaluate(`document.querySelector("button[aria-label='Cancel the active task']").click()`, nil),
		chromedp.Poll(
			`document.querySelector(".state-canceled") !== null`,
			nil,
			chromedp.WithPollingInterval(50*time.Millisecond),
			chromedp.WithPollingTimeout(pollTimeout),
		),
	); err != nil {
		return fmt.Errorf("%s: cancel active task: %w", pageURL, err)
	}
	var cancelDisabled bool
	if err := page.run(chromedp.Evaluate(
		`document.querySelector("button[aria-label='Cancel the active task']")?.disabled === true`,
		&cancelDisabled,
	)); err != nil {
		return fmt.Errorf("%s: inspect canceled task control: %w", pageURL, err)
	}
	if !cancelDisabled {
		return fmt.Errorf("%s: canceled task must disable cancellation", pageURL)
	}

	if err := page.run(chromedp.Evaluate("("+injectApprovalTaskJS+")()", nil)); err != nil {
		return fmt.Errorf("%s: inject approval task: %w", pageURL, err)
	}
	var confirmation struct {
		RationaleActive   bool `json:"rationaleActive"`
		RationaleRequired bool `json:"rationaleRequired"`
		ApproveCount      int  `json:"approveCount"`
		DenyCount         int  `json:"denyCount"`
	}
	if err := page.run(chromedp.Evaluate(`(() => {
    const form = document.querySelector(".confirm form");
    if (!form) throw new Error("approval form is missing");
    const rationale = form.querySelector("input[type='text']");
    const buttons = [...form.querySelectorAll("button")];
    const approve = buttons.filter(button => button.textContent.trim() === "Approve");
    const deny = buttons.filter(button => button.textContent.trim() === "Deny");
    if (deny.length === 1) deny[0].id = "browser-acceptance-deny";
    return {
      rationaleActive: rationale === document.activeElement,
      rationaleRequired: Boolean(rationale?.required),
      approveCount: approve.length,
      denyCount: deny.length,
    };
  })()`, &confirmation)); err != nil {
		return fmt.Errorf("%s: inspect approval controls: %w", pageURL, err)
	}
	if !confirmation.RationaleActive || !confirmation.RationaleRequired {
		return fmt.Errorf("%s: approval rationale must be required and receive focus", pageURL)
	}
	if confirmation.ApproveCount != 1 || confirmation.DenyCount != 1 {
		return fmt.Errorf("%s: approval and denial actions must each exist exactly once", pageURL)
	}
	if err := page.run(
		chromedp.Evaluate(`document.getElementById("browser-acceptance-deny").click()`, nil),
		chromedp.Poll(
			`document.querySelector(".state-completed") !== null`,
			nil,
			chromedp.WithPollingInterval(50*time.Millisecond),
			chromedp.WithPollingTimeout(pollTimeout),
		),
	); err != nil {
		return fmt.Errorf("%s: deny approval: %w", pageURL, err)
	}
	if err := rpc.verify(); err != nil {
		return fmt.Errorf("%s: fake RPC contract: %w", pageURL, err)
	}
	return nil
}

func forcedColorsSmoke(page *browserPage, pageURL string) error {
	if err := openPage(page, pageURL); err != nil {
		return err
	}
	var border struct {
		Style  string `json:"style"`
		Width  string `json:"width"`
		Active bool   `json:"active"`
	}
	if err := page.run(chromedp.Evaluate(`(() => {
    const badge = document.createElement("span");
    badge.className = "state state-working";
    badge.textContent = "working";
    document.body.append(badge);
    const style = getComputedStyle(badge);
    return {
      active: matchMedia("(forced-colors: active)").matches,
      style: style.borderStyle,
      width: style.borderTopWidth,
    };
  })()`, &border)); err != nil {
		return fmt.Errorf("%s: inspect forced-colors fallback: %w", pageURL, err)
	}
	if !border.Active {
		return fmt.Errorf("%s: forced colors is not active", pageURL)
	}
	if border.Style != "solid" || border.Width != "2px" {
		return fmt.Errorf("%s: forced-colors border fallback is missing", pageURL)
	}
	return nil
}

func addClientContrastBadges(page *browserPage) error {
	return page.run(chromedp.Evaluate(`(() => ["working", "input-required", "completed"].forEach(state => {
    const badge = document.createElement("span");
    badge.id = `+"`"+`contrast-${state}`+"`"+`;
    badge.className = `+"`"+`state state-${state}`+"`"+`;
    badge.textContent = state;
    document.body.append(badge);
  }))()`, nil))
}
