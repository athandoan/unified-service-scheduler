/* Client stub: Catalog + Booking only. Never calls /occupations. */
(function () {
  const FALLBACK_FIXTURES = {
    customers: [
      { id: "55555555-5555-4555-8555-555555555555", name: "Ada Customer" },
      { id: "66666666-6666-4666-8666-666666666666", name: "Ben Customer" },
    ],
    dealerships: [
      {
        id: "11111111-1111-4111-8111-111111111111",
        name: "North Workshop",
        timezone: "Europe/London",
        openingHours: [
          { weekday: 1, openMinutes: 480, closeMinutes: 1020 },
          { weekday: 2, openMinutes: 480, closeMinutes: 1020 },
          { weekday: 3, openMinutes: 480, closeMinutes: 1020 },
          { weekday: 4, openMinutes: 480, closeMinutes: 1020 },
          { weekday: 5, openMinutes: 480, closeMinutes: 1020 },
        ],
      },
      {
        id: "22222222-2222-4222-8222-222222222222",
        name: "South Workshop",
        timezone: "Europe/Berlin",
        openingHours: [
          { weekday: 1, openMinutes: 420, closeMinutes: 1080 },
          { weekday: 2, openMinutes: 420, closeMinutes: 1080 },
          { weekday: 3, openMinutes: 420, closeMinutes: 1080 },
          { weekday: 4, openMinutes: 420, closeMinutes: 1080 },
          { weekday: 5, openMinutes: 420, closeMinutes: 1080 },
        ],
      },
    ],
    serviceTypes: [
      { id: "33333333-3333-4333-8333-333333333333", name: "Oil change", durationMinutes: 30 },
      { id: "44444444-4444-4444-8444-444444444444", name: "Annual service", durationMinutes: 60 },
    ],
    vehicles: [
      { id: "77777777-7777-4777-8777-777777777777", customerId: "55555555-5555-4555-8555-555555555555", vin: "WVWZZZ3CZWE000001" },
      { id: "88888888-8888-4888-8888-888888888888", customerId: "55555555-5555-4555-8555-555555555555", vin: "WVWZZZ3CZWE000002" },
      { id: "99999999-9999-4999-8999-999999999999", customerId: "66666666-6666-4666-8666-666666666666", vin: "WBA12345678900001" },
    ],
  };

  let fixtures = FALLBACK_FIXTURES;
  const mockAppointments = new Map();
  const mockIdempotency = new Map();

  const el = (id) => document.getElementById(id);

  function uuid() {
    if (crypto.randomUUID) return crypto.randomUUID();
    return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (c) => {
      const r = (Math.random() * 16) | 0;
      return (c === "x" ? r : (r & 0x3) | 0x8).toString(16);
    });
  }

  function mode() {
    const checked = document.querySelector('input[name="mode"]:checked');
    return checked ? checked.value : "mock";
  }

  function selectedService() {
    const id = el("serviceType").value;
    return (fixtures.serviceTypes || []).find((s) => s.id === id);
  }

  function catalogBody() {
    return {
      dealershipId: el("dealership").value,
      customerId: el("customer").value,
      vehicleId: el("vehicle").value,
      serviceTypeId: el("serviceType").value,
      start: el("start").value.trim(),
    };
  }

  function fillSelect(select, items, labelFn, valueFn) {
    const current = select.value;
    select.innerHTML = "";
    items.forEach((item) => {
      const opt = document.createElement("option");
      opt.value = valueFn(item);
      opt.textContent = labelFn(item);
      select.appendChild(opt);
    });
    if ([...select.options].some((o) => o.value === current)) select.value = current;
  }

  function populateCustomers() {
    fillSelect(
      el("customer"),
      fixtures.customers || [],
      (c) => (c.name ? c.name + " (" + c.id + ")" : c.id),
      (c) => c.id
    );
  }

  function populateDealerships(list) {
    fillSelect(
      el("dealership"),
      list || [],
      (d) => (d.name || d.id) + (d.timezone ? " [" + d.timezone + "]" : ""),
      (d) => d.id
    );
  }

  function populateServiceTypes(list) {
    fillSelect(
      el("serviceType"),
      list || [],
      (s) => (s.name || s.id) + " — " + s.durationMinutes + " min",
      (s) => s.id
    );
    showDuration();
  }

  function populateVehicles(list) {
    fillSelect(
      el("vehicle"),
      list || [],
      (v) => (v.vin || v.id) + " (" + v.id + ")",
      (v) => v.id
    );
  }

  function vehiclesForCustomer(customerId) {
    return (fixtures.vehicles || []).filter((v) => v.customerId === customerId);
  }

  function showDuration() {
    const s = selectedService();
    el("durationDisplay").textContent = s
      ? "Catalog durationMinutes: " + s.durationMinutes + " (display/typical; not sent on write; live occupancy uses technician skill, not this number)"
      : "Catalog durationMinutes: (none)";
  }

  function showRequest(method, url, headers, body) {
    el("requestOut").textContent = JSON.stringify({ method, url, headers: headers || {}, body: body || null }, null, 2);
  }

  function showResponse(status, body) {
    el("responseOut").textContent = JSON.stringify({ status, body }, null, 2);
  }

  function parsePath(path) {
    const q = path.indexOf("?");
    return q === -1 ? { pathname: path, search: "" } : { pathname: path.slice(0, q), search: path.slice(q + 1) };
  }

  function jsonResponse(status, body) {
    return { status, body };
  }

  function addMinutesIso(startIso, minutes) {
    const ms = Date.parse(startIso);
    if (Number.isNaN(ms)) return null;
    return new Date(ms + minutes * 60 * 1000).toISOString().replace(/\.\d{3}Z$/, "Z");
  }

  // Mock stand-in only: derives end from catalogue durationMinutes (no tech picker); live Booking uses technician skill.
  function mockEndAt(start, serviceTypeId) {
    const svc = (fixtures.serviceTypes || []).find((s) => s.id === serviceTypeId);
    if (!svc) return null;
    return addMinutesIso(start, svc.durationMinutes);
  }

  function mockCreateAppointment(fields, status) {
    const end_at = mockEndAt(fields.start, fields.serviceTypeId);
    if (!end_at) return jsonResponse(400, { error: "invalid", message: "unknown serviceTypeId or start" });
    const now = Date.now();
    const row = {
      id: uuid(),
      dealershipId: fields.dealershipId,
      customerId: fields.customerId,
      vehicleId: fields.vehicleId,
      serviceTypeId: fields.serviceTypeId,
      technicianId: uuid(),
      serviceBayId: uuid(),
      start_at: fields.start,
      end_at,
      status,
    };
    if (status === "HELD") {
      row.hold_expires_at = new Date(now + 120 * 1000).toISOString().replace(/\.\d{3}Z$/, "Z");
    }
    mockAppointments.set(row.id, row);
    return jsonResponse(201, row);
  }

  function mockApi(method, path, { body, headers }) {
    const { pathname } = parsePath(path);
    const vehiclesMatch = pathname.match(/^\/customers\/([^/]+)\/vehicles$/);
    const apptMatch = pathname.match(/^\/appointments\/([^/]+)$/);

    if (method === "GET" && pathname === "/dealerships") {
      return jsonResponse(200, fixtures.dealerships || []);
    }
    if (method === "GET" && pathname === "/service-types") {
      return jsonResponse(200, fixtures.serviceTypes || []);
    }
    if (method === "GET" && vehiclesMatch) {
      const customerId = decodeURIComponent(vehiclesMatch[1]);
      const known = (fixtures.customers || []).some((c) => c.id === customerId);
      if (!known) return jsonResponse(404, { error: "not_found", message: "customer" });
      return jsonResponse(200, vehiclesForCustomer(customerId));
    }
    if (method === "POST" && pathname === "/holds") {
      if (!body || !body.start || !body.dealershipId || !body.customerId || !body.vehicleId || !body.serviceTypeId) {
        return jsonResponse(400, { error: "invalid", message: "dealershipId, customerId, vehicleId, serviceTypeId, start required" });
      }
      const payload = {
        dealershipId: body.dealershipId,
        customerId: body.customerId,
        vehicleId: body.vehicleId,
        serviceTypeId: body.serviceTypeId,
        start: body.start,
      };
      return mockCreateAppointment(payload, "HELD");
    }
    if (method === "POST" && pathname === "/appointments") {
      const key = headers && headers["Idempotency-Key"];
      if (!key) return jsonResponse(400, { error: "invalid", message: "Idempotency-Key required on confirm" });
      const fingerprint = JSON.stringify(body || {});
      if (mockIdempotency.has(key)) {
        const prev = mockIdempotency.get(key);
        if (prev.fingerprint !== fingerprint) {
          return jsonResponse(409, { error: "idempotency_mismatch", message: "same key, different body" });
        }
        return jsonResponse(200, prev.appointment);
      }
      let created;
      if (body && body.holdId) {
        const held = mockAppointments.get(body.holdId);
        if (!held) return jsonResponse(409, { error: "unavailable", message: "hold not found" });
        if (held.status !== "HELD") return jsonResponse(409, { error: "unavailable", message: "hold not HELD" });
        if (held.hold_expires_at && Date.parse(held.hold_expires_at) <= Date.now()) {
          return jsonResponse(409, { error: "unavailable", message: "hold expired" });
        }
        held.status = "CONFIRMED";
        delete held.hold_expires_at;
        created = { status: 201, body: held };
      } else {
        if (!body || !body.start || !body.dealershipId || !body.customerId || !body.vehicleId || !body.serviceTypeId) {
          return jsonResponse(400, { error: "invalid", message: "confirm-without-hold requires Catalog ids + start" });
        }
        created = mockCreateAppointment(
          {
            dealershipId: body.dealershipId,
            customerId: body.customerId,
            vehicleId: body.vehicleId,
            serviceTypeId: body.serviceTypeId,
            start: body.start,
          },
          "CONFIRMED"
        );
      }
      if (created.status >= 400) return created;
      mockIdempotency.set(key, { fingerprint, appointment: created.body });
      return created;
    }
    if (method === "GET" && apptMatch) {
      const row = mockAppointments.get(decodeURIComponent(apptMatch[1]));
      if (!row) return jsonResponse(404, { error: "not_found", message: "appointment" });
      return jsonResponse(200, row);
    }
    if (method === "DELETE" && apptMatch) {
      const row = mockAppointments.get(decodeURIComponent(apptMatch[1]));
      if (!row) return jsonResponse(404, { error: "not_found", message: "appointment" });
      row.status = "CANCELLED";
      delete row.hold_expires_at;
      return jsonResponse(204, null);
    }
    return jsonResponse(404, { error: "not_found", message: pathname });
  }

  async function api(method, path, { body, headers } = {}) {
    const hdrs = Object.assign({}, headers || {});
    if (body !== undefined) hdrs["Content-Type"] = "application/json";
    const url = mode() === "live" ? el("baseUrl").value.replace(/\/$/, "") + path : path;
    showRequest(method, url, hdrs, body === undefined ? null : body);

    if (mode() === "mock") {
      const result = mockApi(method, path, { body, headers: hdrs });
      showResponse(result.status, result.body);
      return result;
    }

    try {
      const init = { method, headers: hdrs };
      if (body !== undefined) init.body = JSON.stringify(body);
      const res = await fetch(url, init);
      const text = await res.text();
      let parsed = null;
      if (text) {
        try {
          parsed = JSON.parse(text);
        } catch (e) {
          parsed = text;
        }
      }
      showResponse(res.status, parsed);
      return { status: res.status, body: parsed };
    } catch (err) {
      const fail = { error: "network", message: String(err && err.message ? err.message : err) };
      showResponse(0, fail);
      return { status: 0, body: fail };
    }
  }

  function rememberAppointment(result) {
    if (result && result.body && result.body.id) el("appointmentId").value = result.body.id;
  }

  async function reloadCatalog() {
    const dealers = await api("GET", "/dealerships");
    if (dealers.status === 200 && Array.isArray(dealers.body)) populateDealerships(dealers.body);
    const types = await api("GET", "/service-types");
    if (types.status === 200 && Array.isArray(types.body)) populateServiceTypes(types.body);
    const customerId = el("customer").value;
    if (!customerId) return;
    const vehicles = await api("GET", "/customers/" + encodeURIComponent(customerId) + "/vehicles");
    if (vehicles.status === 200 && Array.isArray(vehicles.body)) populateVehicles(vehicles.body);
  }

  async function postHold() {
    const body = catalogBody();
    const result = await api("POST", "/holds", { body });
    rememberAppointment(result);
  }

  async function postConfirm() {
    const body = catalogBody();
    const result = await api("POST", "/appointments", {
      body,
      headers: { "Idempotency-Key": el("idempotencyKey").value.trim() },
    });
    rememberAppointment(result);
  }

  async function postPromote() {
    const holdId = el("appointmentId").value.trim();
    const result = await api("POST", "/appointments", {
      body: { holdId },
      headers: { "Idempotency-Key": el("idempotencyKey").value.trim() },
    });
    rememberAppointment(result);
  }

  async function getAppointment() {
    const id = el("appointmentId").value.trim();
    await api("GET", "/appointments/" + encodeURIComponent(id));
  }

  async function cancelAppointment() {
    const id = el("appointmentId").value.trim();
    await api("DELETE", "/appointments/" + encodeURIComponent(id));
  }

  async function loadFixtures() {
    try {
      const res = await fetch("fixtures.json");
      if (res.ok) {
        const data = await res.json();
        if (data && typeof data === "object") return data;
      }
    } catch (e) {
      /* file:// or missing file: use embedded fixtures */
    }
    return FALLBACK_FIXTURES;
  }

  function regenKey() {
    el("idempotencyKey").value = uuid();
  }

  async function init() {
    // Served from a remote host (FE domain): point the live-mode Base URL at
    // the BE API instead of localhost. Mock stays the checked default mode.
    const servedHost = window.location.hostname;
    if (servedHost && servedHost !== "localhost" && servedHost !== "127.0.0.1") {
      el("baseUrl").value = "https://api.rjx.dedyn.io";
    }
    fixtures = await loadFixtures();
    populateCustomers();
    populateDealerships(fixtures.dealerships || []);
    populateServiceTypes(fixtures.serviceTypes || []);
    populateVehicles(vehiclesForCustomer(el("customer").value));
    regenKey();

    el("customer").addEventListener("change", () => {
      populateVehicles(vehiclesForCustomer(el("customer").value));
    });
    el("serviceType").addEventListener("change", showDuration);
    el("reloadCatalog").addEventListener("click", reloadCatalog);
    el("hold").addEventListener("click", postHold);
    el("confirm").addEventListener("click", postConfirm);
    el("promote").addEventListener("click", postPromote);
    el("getAppt").addEventListener("click", getAppointment);
    el("cancelAppt").addEventListener("click", cancelAppointment);
    el("regenKey").addEventListener("click", regenKey);
  }

  init();
})();
