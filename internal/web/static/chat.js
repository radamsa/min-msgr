// Web chat client. The page shell is rendered by the server; all reads and
// writes go through the WebSocket, as required by the project plan.
(function () {
	"use strict";

	const root = document.getElementById("chat");
	if (!root) {
		return;
	}
	const chatID = Number(root.dataset.chatId);
	const maxBytes = Number(root.dataset.maxBytes);
	const messagesEl = document.getElementById("messages");
	const statusEl = document.getElementById("chat-status");
	const form = document.getElementById("send-form");
	const bodyEl = document.getElementById("message-body");
	const sendButton = document.getElementById("send-button");
	const loadOlderButton = document.getElementById("load-older");

	const seen = new Set();
	let socket = null;
	let reconnectDelay = 500;
	let nextCursor = null;
	let lastCreatedAtMs = 0;
	let firstConnect = true;
	let deleted = false;
	let pending = new Map();

	function setStatus(text, error) {
		statusEl.textContent = text;
		statusEl.className = error ? "error" : "muted";
	}

	function sanitizeHTML(html) {
		// The server already sanitizes Markdown output. This second allowlist
		// pass keeps the client safe even if a malformed message arrives.
		const allowed = new Set([
			"P", "BR", "STRONG", "EM", "DEL", "CODE", "PRE",
			"BLOCKQUOTE", "UL", "OL", "LI", "H1", "H2", "H3",
			"H4", "H5", "H6", "HR"
		]);
		const template = document.createElement("template");
		template.innerHTML = html;
		const walker = document.createTreeWalker(template.content, NodeFilter.SHOW_ELEMENT);
		const remove = [];
		while (walker.nextNode()) {
			const node = walker.currentNode;
			if (!allowed.has(node.tagName)) {
				remove.push(node);
				continue;
			}
			for (const attr of Array.from(node.attributes)) {
				if (node.tagName === "CODE" && attr.name === "class" &&
					/^language-[a-zA-Z0-9_+-]+$/.test(attr.value)) {
					continue;
				}
				node.removeAttribute(attr.name);
			}
		}
		for (const node of remove) {
			node.replaceWith(...node.childNodes);
		}
		return template.innerHTML;
	}

	function renderMessage(message) {
		if (seen.has(message.seq)) {
			return null;
		}
		seen.add(message.seq);

		const article = document.createElement("article");
		article.className = "message";
		article.dataset.seq = String(message.seq);

		const header = document.createElement("div");
		const author = document.createElement("span");
		author.className = "message-author";
		author.textContent = message.author;
		const time = document.createElement("time");
		time.className = "message-time";
		time.dateTime = message.created_at;
		time.textContent = new Date(message.created_at).toLocaleString();
		header.append(author, time);

		const body = document.createElement("div");
		body.className = "message-body";
		body.innerHTML = sanitizeHTML(message.html || "");

		article.append(header, body);
		return article;
	}

	function appendMessages(messages, prepend) {
		const nodes = [];
		for (const message of messages) {
			const node = renderMessage(message);
			if (node) {
				nodes.push(node);
			}
			const createdAt = Date.parse(message.created_at);
			if (createdAt > lastCreatedAtMs) {
				lastCreatedAtMs = createdAt;
			}
		}
		if (nodes.length === 0) {
			return;
		}
		if (prepend) {
			messagesEl.prepend(...nodes);
		} else {
			messagesEl.append(...nodes);
			messagesEl.scrollTop = messagesEl.scrollHeight;
		}
	}

	function send(payload) {
		if (!socket || socket.readyState !== WebSocket.OPEN) {
			return false;
		}
		socket.send(JSON.stringify(payload));
		return true;
	}

	function subscribe() {
		send({
			type: "messages.subscribe",
			payload: {
				chat_id: chatID,
				since: lastCreatedAtMs ? new Date(lastCreatedAtMs).toISOString() : "",
				initial: firstConnect
			}
		});
		firstConnect = false;
	}

	function connect() {
		if (deleted) {
			return;
		}
		const scheme = location.protocol === "https:" ? "wss:" : "ws:";
		socket = new WebSocket(scheme + "//" + location.host + "/ws?chat_id=" + encodeURIComponent(chatID));

		socket.addEventListener("open", function () {
			reconnectDelay = 500;
			setStatus("Подключено");
			subscribe();
			for (const entry of pending.values()) {
				send({ type: "message.send", payload: entry.payload });
			}
		});

		socket.addEventListener("message", function (event) {
			let incoming;
			try {
				incoming = JSON.parse(event.data);
			} catch (err) {
				setStatus("Некорректный ответ сервера", true);
				return;
			}
			handleEvent(incoming);
		});

		socket.addEventListener("close", function () {
			if (deleted) {
				return;
			}
			setStatus("Соединение потеряно, переподключение…", true);
			window.setTimeout(connect, reconnectDelay);
			reconnectDelay = Math.min(reconnectDelay * 2, 10000);
		});

		socket.addEventListener("error", function () {
			socket.close();
		});
	}

	function handleEvent(event) {
		switch (event.type) {
		case "messages.page":
			appendMessages(event.payload.messages || [], false);
			nextCursor = event.payload.next_cursor || null;
			loadOlderButton.hidden = !nextCursor;
			break;
		case "message.created":
			appendMessages([event.payload.message], false);
			break;
		case "message.ack":
			pending.delete(event.payload.client_message_id);
			break;
		case "message.error":
			pending.delete(event.payload.client_message_id);
			setStatus(event.payload.message || "Не удалось отправить сообщение", true);
			break;
		case "history.trimmed":
			removeTrimmed(event.payload.through_seq);
			break;
		case "chat.deleted":
			deleted = true;
			form.hidden = true;
			setStatus("Чат удалён, ключ отозван. История сохранена в архиве.", true);
			if (socket) {
				socket.close();
			}
			break;
		default:
			break;
		}
	}

	function removeTrimmed(throughSeq) {
		for (const node of Array.from(messagesEl.children)) {
			if (Number(node.dataset.seq) <= throughSeq) {
				seen.delete(Number(node.dataset.seq));
				node.remove();
			}
		}
	}

	form.addEventListener("submit", function (event) {
		event.preventDefault();
		const body = bodyEl.value.trim();
		if (!body) {
			return;
		}
		if (new TextEncoder().encode(body).length > maxBytes) {
			setStatus("Сообщение слишком длинное", true);
			return;
		}
		const clientMessageID = (crypto.randomUUID && crypto.randomUUID()) ||
			String(Date.now()) + "-" + Math.random().toString(16).slice(2);
		const payload = { chat_id: chatID, body: body, client_message_id: clientMessageID };
		pending.set(clientMessageID, { payload: payload });
		if (!send({ type: "message.send", payload: payload })) {
			setStatus("Нет соединения, сообщение будет отправлено после переподключения", true);
			return;
		}
		bodyEl.value = "";
		setStatus("Отправка…");
	});

	loadOlderButton.addEventListener("click", function () {
		if (!nextCursor) {
			return;
		}
		if (!send({ type: "messages.before", payload: { chat_id: chatID, cursor: nextCursor } })) {
			setStatus("Нет соединения", true);
			return;
		}
		loadOlderButton.hidden = true;
	});

	connect();
})();
