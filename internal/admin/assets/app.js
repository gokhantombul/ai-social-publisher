document.addEventListener("htmx:configRequest", function (event) {
  var meta = document.querySelector('meta[name="csrf-token"]');
  if (meta) event.detail.headers["X-CSRF-Token"] = meta.content;
});

document.addEventListener("htmx:responseError", function (event) {
  if (event.detail.xhr.status === 401) window.location.assign("/login");
});

