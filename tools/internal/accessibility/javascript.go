package accessibility

const contrastColorsJS = `function(selector) {
  const element = document.querySelector(selector);
  if (!element) throw new Error(` + "`" + `missing contrast target: ${selector}` + "`" + `);
  const canvas = document.createElement("canvas");
  canvas.width = canvas.height = 1;
  const context = canvas.getContext("2d", { willReadFrequently: true });
  const rgba = value => {
    context.clearRect(0, 0, 1, 1);
    context.fillStyle = value;
    context.fillRect(0, 0, 1, 1);
    return [...context.getImageData(0, 0, 1, 1).data];
  };
  const over = (top, bottom) => {
    const alpha = top[3] / 255;
    return [
      top[0] * alpha + bottom[0] * (1 - alpha),
      top[1] * alpha + bottom[1] * (1 - alpha),
      top[2] * alpha + bottom[2] * (1 - alpha),
      255,
    ];
  };
  const backgrounds = [];
  for (let node = element; node; node = node.parentElement) {
    backgrounds.push(rgba(getComputedStyle(node).backgroundColor));
  }
  const background = backgrounds.reverse().reduce((color, layer) => over(layer, color), [255, 255, 255, 255]);
  const foreground = over(rgba(getComputedStyle(element).color), background);
  return { foreground: foreground.slice(0, 3), background: background.slice(0, 3) };
}`

const injectWorkingTaskJS = `() => {
  state.baseUrl = location.origin;
  handleResult({
    kind: "status-update",
    contextId: "browser-acceptance",
    taskId: "task-cancel",
    status: { state: "working", message: { parts: [{ kind: "text", text: "Working" }] } },
  });
}`

const injectApprovalTaskJS = `() => handleResult({
  kind: "status-update",
  contextId: "browser-acceptance",
  taskId: "task-approval",
  status: {
    state: "input-required",
    message: { parts: [{
      kind: "data",
      data: {
        id: "approval-call",
        name: "adk_request_confirmation",
        args: {
          originalFunctionCall: { name: "restart_service", args: { service: "api" } },
          toolConfirmation: { hint: "Review the restart evidence." },
        },
      },
      metadata: { adk_type: "function_call" },
    }] },
  },
})`
