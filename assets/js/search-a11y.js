// Give Hextra's FlexSearch box the combobox semantics a screen reader needs.
//
// Hextra renders a bare `<input type="search">` with an `aria-label`, a detached results `<ul>`, and a polite status
// region, but nothing ties them together: a screen-reader user is never told that results
// appeared or how many. The pattern below is the WAI-ARIA editable combobox with list autocomplete.
//
// The native browser acceptance gate holds this behavior honest.

const RESULTS_ID = "hextra-search-results";

// Hextra renders the box twice — once in the navbar, once in the small-screen sidebar — so
// the ids are numbered. Reusing one id across both would put a duplicate id in the document
// and leave one aria-controls pointing at the wrong list.
let wrapperCount = 0;

function connectSearch(wrapper) {
  const input = wrapper.querySelector("input[type='search']");
  const results = wrapper.querySelector("ul.hextra-search-results");
  if (!input || !results || input.dataset.accessibilityReady) return;

  wrapperCount += 1;
  results.id ||= wrapperCount === 1 ? RESULTS_ID : `${RESULTS_ID}-${wrapperCount}`;
  results.setAttribute("role", "listbox");
  input.setAttribute("role", "combobox");
  input.setAttribute("aria-controls", results.id);
  input.setAttribute("aria-autocomplete", "list");
  input.setAttribute("aria-expanded", "false");
  input.setAttribute("aria-haspopup", "listbox");
  input.dataset.accessibilityReady = "true";

  // Hextra renders results by replacing the list's children, so the expanded state is derived
  // from the DOM rather than from a keystroke — that keeps it correct when a query returns
  // nothing, and when the list is dismissed by a click outside.
  const sync = () => {
    const open = !results.classList.contains("hx:hidden") && results.querySelector("a") !== null;
    input.setAttribute("aria-expanded", String(open));
  };
  new MutationObserver(sync).observe(results, {
    childList: true,
    subtree: true,
    attributes: true,
    attributeFilter: ["class"],
  });
  sync();
}

function connectAll() {
  document.querySelectorAll(".hextra-search-wrapper").forEach(connectSearch);
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", connectAll);
} else {
  connectAll();
}
