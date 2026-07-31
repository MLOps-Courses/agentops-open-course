// The locked Zensical renderer exposes its search UI through an open shadow
// root. Pin this reviewed compatibility boundary so a renderer bump must
// revalidate the shim before it can ship.
const SEARCH_SHIM_ZENSICAL = "0.0.52";

function repairSearchAccessibility(root) {
    const input = root.querySelector('input[role="combobox"]');
    if (!input || input.dataset.accessibilityReady) return;

    const buttons = root.querySelectorAll("button");
    const results = root.querySelector("ol");
    if (buttons[0]) buttons[0].setAttribute("aria-label", "Close search");
    if (buttons[1]) buttons[1].setAttribute("aria-label", "Filter search results");

    if (results) {
        results.id ||= "zensical-search-results";
        input.setAttribute("aria-controls", results.id);
    }
    input.setAttribute("aria-expanded", "false");
    input.setAttribute("aria-autocomplete", "list");
    input.dataset.accessibilityReady = "true";

    const syncExpanded = () => {
        input.setAttribute("aria-expanded", String(Boolean(results?.querySelector("a"))));
    };
    input.addEventListener("input", () => requestAnimationFrame(syncExpanded));
    if (results) new MutationObserver(syncExpanded).observe(results, { childList: true, subtree: true });
}

function repairAccessibility() {
    const generator = document.querySelector('meta[name="generator"]')?.content;
    if (generator !== `zensical-${SEARCH_SHIM_ZENSICAL}`) return;

    document.querySelectorAll('input.md-option[aria-hidden="true"]').forEach((input) => {
        input.disabled = true;
    });
    document.querySelectorAll("*").forEach((element) => {
        if (element.shadowRoot) repairSearchAccessibility(element.shadowRoot);
    });
}

repairAccessibility();
new MutationObserver(repairAccessibility).observe(document.body, { childList: true, subtree: true });
