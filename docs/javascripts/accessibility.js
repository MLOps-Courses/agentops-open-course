// The locked Zensical renderer exposes its search UI through an open shadow
// root. Pin this reviewed compatibility boundary so a renderer bump must
// revalidate the shim before it can ship.
const SEARCH_SHIM_ZENSICAL = "0.0.65";
const observedSearchRoots = new WeakSet();

function repairSearchAccessibility(root) {
    const input = root.querySelector('input[role="combobox"]');
    const buttons = root.querySelectorAll("button");
    const results = root.querySelector("ol");
    if (!input || buttons.length < 2 || !results) {
        if (!observedSearchRoots.has(root)) {
            observedSearchRoots.add(root);
            // Zensical hydrates its open shadow root after the host reaches the DOM.
            new MutationObserver(() => repairSearchAccessibility(root)).observe(root, {
                childList: true,
                subtree: true,
            });
        }
        return;
    }
    if (input.dataset.accessibilityReady) return;

    buttons[0].setAttribute("aria-label", "Close search");
    buttons[1].setAttribute("aria-label", "Filter search results");

    results.id ||= "zensical-search-results";
    input.setAttribute("aria-controls", results.id);
    input.setAttribute("aria-expanded", "false");
    input.setAttribute("aria-autocomplete", "list");
    input.dataset.accessibilityReady = "true";

    const syncExpanded = () => {
        input.setAttribute("aria-expanded", String(Boolean(results.querySelector("a"))));
    };
    input.addEventListener("input", () => requestAnimationFrame(syncExpanded));
    new MutationObserver(syncExpanded).observe(results, { childList: true, subtree: true });
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
