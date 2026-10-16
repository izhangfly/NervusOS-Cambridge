package com.example.hello

import com.nervus.sdk.annotations.Interface
import com.nervus.sdk.annotations.Method

@Interface(id = "com.example.hello.Greeter")
interface Greeter {
    @Method(id = 1)
    suspend fun greet(name: String): String
}
