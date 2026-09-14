"""Offline protocol proof for the elective, using the same A2A request as a client."""

import unittest

from starlette.testclient import TestClient

from labs.langgraph_a2a import build_graph, create_app


class Interoperability(unittest.TestCase):
    def test_graph_rejects_untrusted_identifier(self):
        result = build_graph().invoke({"incident_id": "' OR 1=1 --"})
        self.assertIn("Invalid incident ID", result["answer"])

    def test_a2a_returns_the_graph_evidence(self):
        expected = build_graph().invoke({"incident_id": "INC-002"})["answer"]
        with TestClient(create_app()) as client:
            self.assertEqual(client.get("/.well-known/agent-card.json").status_code, 200)
            response = client.post(
                "/",
                json={
                    "jsonrpc": "2.0",
                    "id": "comparison-1",
                    "method": "message/send",
                    "params": {
                        "message": {
                            "role": "user",
                            "messageId": "comparison-message",
                            "parts": [{"kind": "text", "text": "INC-002"}],
                        }
                    },
                },
            )
            self.assertEqual(response.status_code, 200)
            body = response.json()
            self.assertNotIn("error", body)
            self.assertEqual(body["result"]["artifacts"][0]["parts"][0]["text"], expected)


if __name__ == "__main__":
    unittest.main()
