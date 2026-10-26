document.addEventListener('submit', function (event) {
  var form = event.target;
  if (!form || !form.matches || !form.matches('form[data-confirm]')) {
    return;
  }
  if (!window.confirm(form.getAttribute('data-confirm') || 'Lanjutkan?')) {
    event.preventDefault();
  }
});
