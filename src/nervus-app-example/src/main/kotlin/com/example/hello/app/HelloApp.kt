package com.example.hello.app

import androidx.compose.foundation.layout.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.example.hello.Greeter
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.ui.attachComposeDesktop
import kotlinx.coroutines.launch

fun main() {
    val app = HelloApp()
    Runtime.getRuntime().addShutdownHook(Thread { app.close() })
    app.start()
}

class HelloApp : NervusApp(
    config = ComponentConfig(componentId = "hello_app")
) {
    // The Greeter runs in-process for now. Exporting it through the kernel
    // (providedInterfaces + manifest exports) needs a schema hash, and the
    // provider.binpb / schemas.binpb toolchain for user apps doesn't exist yet
    // (nervus-ipc needs a general providergen CLI). Until it does, the kernel
    // rejects RegisterEndpoint with "schema hash does not match", so the app
    // keeps the interface local and stays installable.
    private val greeter: Greeter = object : Greeter {
        override suspend fun greet(name: String): String = "Hello, $name! (from NervusOS)"
    }

    // Uses no system interfaces; connecting to nervud is the whole demo
    override val requiredInterfaces = emptyList<InterfaceRequirement>()

    override fun onReady() {
        logger.info("HelloApp ready, launching UI...")
        attachComposeDesktop(
            title = "Nervus Hello World",
            width = 400.dp,
            height = 300.dp,
        ) {
            HelloScreen(greeter)
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HelloScreen(greeter: Greeter) {
    var name by remember { mutableStateOf("World") }
    var result by remember { mutableStateOf("") }
    var loading by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()

    MaterialTheme {
        Scaffold(
            topBar = {
                TopAppBar(title = { Text("Nervus Hello World") })
            }
        ) { padding ->
            Column(
                modifier = Modifier
                    .fillMaxSize()
                    .padding(padding)
                    .padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp)
            ) {
                OutlinedTextField(
                    value = name,
                    onValueChange = { name = it },
                    label = { Text("Your name") },
                    modifier = Modifier.fillMaxWidth(),
                    singleLine = true,
                )

                Button(
                    onClick = {
                        loading = true
                        result = ""
                        scope.launch {
                            try {
                                val reply = greeter.greet(name)
                                result = reply
                            } catch (e: Exception) {
                                result = "Error: ${e.message}"
                            } finally {
                                loading = false
                            }
                        }
                    },
                    enabled = name.isNotBlank() && !loading,
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    if (loading) {
                        CircularProgressIndicator(
                            modifier = Modifier.size(16.dp),
                            strokeWidth = 2.dp,
                            color = MaterialTheme.colorScheme.onPrimary,
                        )
                        Spacer(Modifier.width(8.dp))
                    }
                    Text("Greet")
                }

                if (result.isNotEmpty()) {
                    Card(
                        modifier = Modifier.fillMaxWidth(),
                        colors = CardDefaults.cardColors(
                            containerColor = MaterialTheme.colorScheme.primaryContainer,
                        ),
                    ) {
                        Text(
                            text = result,
                            modifier = Modifier.padding(16.dp),
                            style = MaterialTheme.typography.bodyLarge,
                        )
                    }
                }
            }
        }
    }
}
