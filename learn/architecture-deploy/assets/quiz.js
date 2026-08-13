/**
 * Minimal quiz helper for lessons.
 * Usage: data-correct="1" on the correct button (0-based index),
 * data-feedback-ok / data-feedback-bad on the .quiz root.
 */
(function () {
  function wireQuiz(root) {
    if (root.dataset.wired) return;
    root.dataset.wired = "1";
    var correct = Number(root.getAttribute("data-correct") || "0");
    var buttons = Array.prototype.slice.call(root.querySelectorAll("button[data-choice]"));
    var feedback = root.querySelector(".feedback");
    buttons.forEach(function (btn, i) {
      btn.addEventListener("click", function () {
        buttons.forEach(function (b) { b.disabled = true; });
        var ok = i === correct;
        if (feedback) {
          feedback.className = "feedback " + (ok ? "ok" : "bad");
          feedback.textContent = ok
            ? (root.getAttribute("data-feedback-ok") || "Correct.")
            : (root.getAttribute("data-feedback-bad") || "Not quite—try the primary reading, then retake.");
        }
      });
    });
  }
  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll(".quiz").forEach(wireQuiz);
  });
})();
