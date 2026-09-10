// A copy control on every command block.
//
// Injected rather than written into the markup, and that is the whole
// design decision here. Hand-placed buttons are one more thing to
// remember on every new snippet, and the ones nobody remembers are
// exactly the long commands worth copying. One rule instead: a block you
// type gets a button, a block that came back does not.
//
// Output blocks are excluded on purpose (.io-out). Nothing pastes engine
// output anywhere, and a Copy button on it invites somebody to paste a
// transcript into a shell.
//
// The button is hidden outright where navigator.clipboard is missing,
// which is any page served over plain http from something that is not
// localhost. A control that silently does nothing is worse than no
// control: the reader thinks they have copied.
(function () {
  "use strict";

  if (!navigator.clipboard || !navigator.clipboard.writeText) return;

  var ICON = "⎘";            // ⎘, the same glyph libviprs.org uses
  var TICK = "✓";            // ✓
  var RESTORE_AFTER = 1500;

  var blocks = document.querySelectorAll("pre:not(.io-out)");

  Array.prototype.forEach.call(blocks, function (pre) {
    // The note bubble carries no command, and wrapping a <pre> inside an
    // absolutely-positioned bubble would reposition it.
    if (pre.closest(".tip-bubble")) return;

    var wrap = document.createElement("div");
    wrap.className = "code-wrap";
    pre.parentNode.insertBefore(wrap, pre);
    wrap.appendChild(pre);

    var button = document.createElement("button");
    button.type = "button";
    button.className = "copy-btn";
    button.innerHTML = ICON + " Copy";
    button.setAttribute("aria-label", "Copy this command to the clipboard");
    wrap.insertBefore(button, pre);
  });

  // One delegated listener rather than one per button: fewer handlers,
  // and it keeps working if a block is ever added after load.
  document.addEventListener("click", function (event) {
    var button = event.target && event.target.closest && event.target.closest(".code-wrap .copy-btn");
    if (!button) return;

    var wrap = button.closest(".code-wrap");
    var code = wrap && wrap.querySelector("pre code");
    if (!code) return;

    navigator.clipboard.writeText(code.textContent).then(function () {
      var previous = button.innerHTML;
      button.innerHTML = TICK + " Copied";
      button.classList.add("is-copied");
      window.setTimeout(function () {
        button.innerHTML = previous;
        button.classList.remove("is-copied");
      }, RESTORE_AFTER);
    }).catch(function () {
      // Permission refused, or a context the browser will not allow it
      // in. Say so rather than pretending it worked.
      var previous = button.innerHTML;
      button.innerHTML = "Copy failed";
      window.setTimeout(function () { button.innerHTML = previous; }, RESTORE_AFTER);
    });
  });
})();
