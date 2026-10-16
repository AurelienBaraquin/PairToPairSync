package io.p2psync.android

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Base64

/** Parses real output of the Go engine (src/test/resources, see mobile/mobile_test.go). */
class ModelsTest {
    private fun fixture(name: String) =
        javaClass.classLoader!!.getResource(name)!!.readText().trim()

    @Test
    fun parsesARunningRelayStatus() {
        val st = parseStatus(fixture("status.json"))
        assertTrue(st.running)
        assertEquals("Pixel", st.deviceName)
        assertTrue(st.deviceId.startsWith("12D3Koo"))
        assertTrue(st.addresses.isNotEmpty())
        assertEquals(1, st.folders.size)
        val f = st.folders[0]
        assertEquals("docs", f.id)
        assertEquals("Documents", f.label)
        assertEquals(512L * 1024 * 1024, f.quotaBytes)
        assertEquals(0, f.pendingChanges)
        assertEquals(1, st.peers.size)
        assertEquals("pc", st.peers[0].name)
        assertFalse(st.peers[0].relayed)
    }

    @Test
    fun aStoppedEngineIsNotRunning() {
        assertFalse(parseStatus("""{"running":false}""").running)
    }

    @Test
    fun parsesTheConfigurationWithoutAnySecret() {
        val raw = fixture("config.json")
        assertFalse("the UI view must never carry a secret", raw.contains("secret", ignoreCase = true))
        val c = parseConfig(raw)
        assertEquals("Pixel", c.deviceName)
        assertFalse(c.lanDiscovery)
        assertEquals(1, c.devices.size)
        val f = c.folders.single()
        assertEquals("relay", f.role)
        assertEquals("Documents", f.title)
        assertEquals(512L, f.quotaMb)
        assertEquals(720, f.retentionHours)
        assertEquals("pc", f.devices.single().name)
    }

    @Test
    fun readsAnInvitationForDisplay() {
        val inv = parseInvite(fixture("invite.txt"))
        assertNotNull(inv)
        inv!!
        assertEquals("pc", inv.inviterName)
        assertEquals("docs", inv.folder)
        assertEquals("Documents", inv.shownFolder)
        assertTrue("created with --as-relay", inv.forRelay)
        assertFalse(inv.expired(nowMs = (inv.expiresAtSec - 5) * 1000))
        assertTrue(inv.expired(nowMs = (inv.expiresAtSec + 5) * 1000))
        assertTrue(inv.code.startsWith(INVITE_PREFIX))
    }

    @Test
    fun acceptsTheBarePayloadAndSurroundingWhitespace() {
        val code = fixture("invite.txt")
        val bare = code.removePrefix(INVITE_PREFIX)
        assertEquals(parseInvite(code)!!.code, parseInvite("  $bare\n")!!.code)
    }

    @Test
    fun aFullCopyInvitationIsRecognisedSoTheAppCanRefuseIt() {
        val payload = fixture("invite.txt").removePrefix(INVITE_PREFIX)
        val obj = JSONObject(String(Base64.getUrlDecoder().decode(payload)))
        obj.put("er", false)
        val code = INVITE_PREFIX + Base64.getUrlEncoder().withoutPadding().encodeToString(obj.toString().toByteArray())
        assertFalse(parseInvite(code)!!.forRelay)
    }

    @Test
    fun rejectsEverythingThatIsNotAnInvitation() {
        for (bad in listOf("", "hello", "p2psync://pair/", "p2psync://pair/!!!", "https://example.com",
            INVITE_PREFIX + Base64.getUrlEncoder().withoutPadding().encodeToString("{}".toByteArray()),
            INVITE_PREFIX + Base64.getUrlEncoder().withoutPadding().encodeToString("""{"v":2,"id":"x","t":"y"}""".toByteArray()))) {
            assertNull("accepted: $bad", parseInvite(bad))
        }
    }

    @Test
    fun formatsSizes() {
        assertEquals("12 B", formatBytes(12))
        assertEquals("1 kB", formatBytes(1_000))
        assertEquals("1.5 MB", formatBytes(1_500_000))
        assertEquals("2.0 GB", formatBytes(2_000_000_000))
    }

    private fun codeWith(mutate: (JSONObject) -> Unit): String {
        val payload = fixture("invite.txt").removePrefix(INVITE_PREFIX)
        val obj = JSONObject(String(Base64.getUrlDecoder().decode(payload)))
        mutate(obj)
        return INVITE_PREFIX + Base64.getUrlEncoder().withoutPadding().encodeToString(obj.toString().toByteArray())
    }

    @Test
    fun hostileTextIsNeutralisedBeforeItReachesTheConfirmationDialog() {
        // A right-to-left override can make "evil" look like something else; control characters can hide text.
        val inv = parseInvite(codeWith {
            it.put("n", "\u202Eevil\u202C PC\u0000\n")
            it.put("l", "x".repeat(500))
        })!!
        assertEquals("evil PC", inv.inviterName)
        assertTrue(inv.label.length <= 48)
        assertTrue(inv.shortId.contains("…") && inv.shortId.length == 13 && inv.shortId.endsWith(inv.inviterId.takeLast(6)))
    }

    @Test
    fun refusesPaddedBase64LikeTheEngineDoes() {
        // Java's decoder tolerates "=", Go's RawURLEncoding does not: do not show a dialog that cannot succeed.
        val padded = fixture("invite.txt") + "="
        assertNull(parseInvite(padded))
    }
}
