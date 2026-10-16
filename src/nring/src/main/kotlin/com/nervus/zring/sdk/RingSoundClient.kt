package com.nervus.zring.sdk

import java.util.UUID
import java.util.concurrent.locks.ReentrantLock
import kotlin.concurrent.withLock
import kotlin.time.Duration
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeout

/**
 * Protocol client over a [RingSoundTransport].
 *
 * Matches requests to responses, queues unsolicited packets per command and
 * dispatches long-lived packet handlers. State is guarded by a lock; critical
 * sections never suspend, mirroring the Swift actor semantics.
 */
class RingSoundClient(
    private val transport: RingSoundTransport,
    val commandTimeout: Duration = RingSoundDefaults.COMMAND_TIMEOUT,
    handlerDispatcher: CoroutineDispatcher = Dispatchers.Default
) {
    private class Waiter(
        val id: UUID,
        val deferred: CompletableDeferred<Packet>
    )

    private val lock = ReentrantLock()
    private val stream = PacketStream()
    private val packetQueues = mutableMapOf<Int, ArrayDeque<Packet>>()
    private val waiters = mutableMapOf<Int, MutableList<Waiter>>()
    private val handlers = mutableMapOf<Int, MutableMap<UUID, suspend (Packet) -> Unit>>()
    private val pendingProtocolErrors = ArrayDeque<RingSoundError>()

    @Volatile
    private var connected = false

    private val handlerScope = CoroutineScope(SupervisorJob() + handlerDispatcher)

    val isConnected: Boolean
        get() = connected && transport.isConnected

    suspend fun connect() {
        resetReceiveState()
        connected = false
        transport.setReceiveHandler { data -> receive(data) }
        transport.setDisconnectHandler { transportDisconnected() }
        try {
            transport.connect()
            connected = true
        } catch (error: Throwable) {
            connected = false
            throw error
        }
    }

    suspend fun disconnect() {
        transport.disconnect()
        transportDisconnected()
    }

    suspend fun sendCommand(command: Int, body: ByteArray = ByteArray(0)) {
        if (!connected || !transport.isConnected) {
            throw RingSoundError.Transport("BLE client is not connected")
        }
        transport.write(RingSoundProtocol.encodePacket(command, body))
    }

    suspend fun sendCommand(command: RingCommand, body: ByteArray = ByteArray(0)) {
        sendCommand(command.value, body)
    }

    suspend fun request(
        command: Int,
        responseCommand: Int,
        body: ByteArray = ByteArray(0),
        timeout: Duration? = null
    ): Packet {
        drainCommandQueue(responseCommand)
        lock.withLock { pendingProtocolErrors.clear() }
        sendCommand(command, body)
        return waitForCommand(responseCommand, timeout)
    }

    suspend fun request(
        command: RingCommand,
        responseCommand: RingCommand,
        body: ByteArray = ByteArray(0),
        timeout: Duration? = null
    ): Packet = request(command.value, responseCommand.value, body, timeout)

    suspend fun waitForCommand(command: Int, timeout: Duration? = null): Packet {
        val waiter = lock.withLock {
            pendingProtocolErrors.removeFirstOrNull()?.let { throw it }
            packetQueues[command]?.removeFirstOrNull()?.let { return it }
            if (!connected || !transport.isConnected) {
                throw RingSoundError.Transport(
                    "BLE disconnected while waiting for command 0x%04X".format(command)
                )
            }
            val waiter = Waiter(UUID.randomUUID(), CompletableDeferred())
            waiters.getOrPut(command) { mutableListOf() }.add(waiter)
            waiter
        }

        val interval = (timeout ?: commandTimeout).coerceAtLeast(Duration.ZERO)
        return try {
            withTimeout(interval) { waiter.deferred.await() }
        } catch (error: TimeoutCancellationException) {
            val removed = lock.withLock { removeWaiterLocked(waiter.id, command) }
            if (removed == null) {
                // The waiter was resumed concurrently (packet, error or disconnect);
                // propagate that outcome instead of a spurious timeout.
                return waiter.deferred.await()
            }
            throw RingSoundError.Timeout(command)
        } catch (error: CancellationException) {
            lock.withLock { removeWaiterLocked(waiter.id, command) }
            throw error
        }
    }

    suspend fun waitForCommand(command: RingCommand, timeout: Duration? = null): Packet =
        waitForCommand(command.value, timeout)

    fun addPacketHandler(
        command: Int,
        handler: suspend (Packet) -> Unit
    ): PacketHandlerToken {
        val token = PacketHandlerToken(command)
        lock.withLock {
            handlers.getOrPut(command) { mutableMapOf() }[token.id] = handler
        }
        return token
    }

    fun addPacketHandler(
        command: RingCommand,
        handler: suspend (Packet) -> Unit
    ): PacketHandlerToken = addPacketHandler(command.value, handler)

    fun removePacketHandler(token: PacketHandlerToken) {
        lock.withLock {
            handlers[token.command]?.remove(token.id)
            if (handlers[token.command]?.isEmpty() == true) {
                handlers.remove(token.command)
            }
        }
    }

    internal fun drainCommandQueue(command: Int) {
        lock.withLock { packetQueues[command] = ArrayDeque() }
    }

    private fun receive(data: ByteArray) {
        lock.withLock {
            try {
                val packets = stream.feed(data)
                for (packet in packets) {
                    routeLocked(packet)
                }
            } catch (error: RingSoundError) {
                stream.clear()
                failForProtocolErrorLocked(error)
            } catch (error: Throwable) {
                stream.clear()
                failForProtocolErrorLocked(
                    RingSoundError.Protocol("Packet stream failed: ${error.message}")
                )
            }
        }
    }

    private fun routeLocked(packet: Packet) {
        val commandWaiters = waiters[packet.command]
        if (!commandWaiters.isNullOrEmpty()) {
            val waiter = commandWaiters.removeAt(0)
            if (commandWaiters.isEmpty()) {
                waiters.remove(packet.command)
            }
            waiter.deferred.complete(packet)
        } else {
            packetQueues.getOrPut(packet.command) { ArrayDeque() }.add(packet)
        }

        handlers[packet.command]?.values?.forEach { handler ->
            handlerScope.launch { handler(packet) }
        }
    }

    private fun removeWaiterLocked(id: UUID, command: Int): Waiter? {
        val commandWaiters = waiters[command] ?: return null
        val index = commandWaiters.indexOfFirst { it.id == id }
        if (index < 0) return null
        val waiter = commandWaiters.removeAt(index)
        if (commandWaiters.isEmpty()) {
            waiters.remove(command)
        }
        return waiter
    }

    private fun failForProtocolErrorLocked(error: RingSoundError) {
        val allWaiters = waiters.values.flatten()
        waiters.clear()
        if (allWaiters.isEmpty()) {
            pendingProtocolErrors.add(error)
        } else {
            allWaiters.forEach { it.deferred.completeExceptionally(error) }
        }
    }

    private fun transportDisconnected() {
        lock.withLock {
            if (!connected && waiters.isEmpty()) return
            connected = false
            stream.clear()
            packetQueues.clear()
            pendingProtocolErrors.clear()
            val allWaiters = waiters.values.flatten()
            waiters.clear()
            allWaiters.forEach {
                it.deferred.completeExceptionally(RingSoundError.Transport("BLE disconnected"))
            }
        }
    }

    private fun resetReceiveState() {
        lock.withLock {
            stream.clear()
            packetQueues.clear()
            pendingProtocolErrors.clear()
            val allWaiters = waiters.values.flatten()
            waiters.clear()
            allWaiters.forEach {
                it.deferred.completeExceptionally(
                    RingSoundError.Transport("BLE connection was reset")
                )
            }
        }
    }
}
